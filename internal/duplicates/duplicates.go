// Package duplicates finds exact and likely duplicate image files without
// modifying them. PerceptualPairs are candidates for human review; callers
// must not treat them as safe automatic deletions.
package duplicates

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

const (
	defaultDHashThreshold       = 12
	defaultAspectRatioTolerance = 0.04
	defaultColorDistance        = 20.0
)

// ImageInput identifies a local copy of a Photos image. Width and Height are
// optional original dimensions; when absent, Scan uses the decoded dimensions
// for keeper selection.
type ImageInput struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Filename string `json:"filename,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	Favorite bool   `json:"favorite,omitempty"`
}

// Options controls the conservative perceptual-candidate filter. Zero values
// select conservative defaults. A negative DHashThreshold disables perceptual
// candidate generation while preserving exact duplicate detection.
type Options struct {
	DHashThreshold       int     `json:"dHashThreshold,omitempty"`
	AspectRatioTolerance float64 `json:"aspectRatioTolerance,omitempty"`
	ColorDistance        float64 `json:"colorDistance,omitempty"`
}

// Image contains input metadata and evidence collected without changing the
// source file.
type Image struct {
	ID             string `json:"id"`
	Path           string `json:"path"`
	Filename       string `json:"filename"`
	Width          int    `json:"width,omitempty"`
	Height         int    `json:"height,omitempty"`
	Favorite       bool   `json:"favorite,omitempty"`
	ByteSize       int64  `json:"byteSize"`
	DecodedWidth   int    `json:"decodedWidth"`
	DecodedHeight  int    `json:"decodedHeight"`
	OriginalSHA256 string `json:"originalSha256"`
	PixelSHA256    string `json:"pixelSha256"`
}

// ExactGroup is an exact match group. Kind is either "original_bytes" or
// "decoded_pixels". Scan omits a pixel group with exactly the same members as
// a byte group. A broader pixel group can still contain a byte-identical subset.
type ExactGroup struct {
	Kind     string  `json:"kind"`
	Images   []Image `json:"images"`
	KeeperID string  `json:"keeperId"`
	Evidence string  `json:"evidenceSha256"`
}

// PerceptualPair is only a candidate for human visual review. Similarity here
// is deliberately never evidence that either source is safe to delete.
type PerceptualPair struct {
	Left                Image   `json:"left"`
	Right               Image   `json:"right"`
	SuggestedKeeperID   string  `json:"suggestedKeeperId"`
	DHashDistance       int     `json:"dHashDistance"`
	AspectRatioDelta    float64 `json:"aspectRatioDelta"`
	ColorDistance       float64 `json:"colorDistance"`
	Classification      string  `json:"classification"`
	SafeForAutoDeletion bool    `json:"safeForAutoDeletion"`
}

// Failure identifies an input that Scan could not inspect.
type Failure struct {
	ID      string `json:"id,omitempty"`
	Path    string `json:"path,omitempty"`
	Stage   string `json:"stage"`
	Message string `json:"message"`
}

type Counts struct {
	Requested            int `json:"requested"`
	Unique               int `json:"unique"`
	Scanned              int `json:"scanned"`
	Failed               int `json:"failed"`
	ByteExactGroups      int `json:"byteExactGroups"`
	PixelExactGroups     int `json:"pixelExactGroups"`
	PerceptualCandidates int `json:"perceptualCandidates"`
}

// Report separates exact evidence from candidates requiring visual review.
type Report struct {
	Counts           Counts           `json:"counts"`
	Images           []Image          `json:"images"`
	ByteExactGroups  []ExactGroup     `json:"byteExactGroups"`
	PixelExactGroups []ExactGroup     `json:"pixelExactGroups"`
	PerceptualPairs  []PerceptualPair `json:"perceptualPairs"`
	Failures         []Failure        `json:"failures"`
}

// ScanError means the returned report is incomplete. Its Report failures give
// the affected IDs and paths.
type ScanError struct {
	Failed int
	Total  int
}

func (e *ScanError) Error() string {
	return fmt.Sprintf("duplicate scan incomplete: %d of %d unique images failed", e.Failed, e.Total)
}

type scannedImage struct {
	public    Image
	byteHash  [sha256.Size]byte
	pixelHash [sha256.Size]byte
	dhash     [32]byte
	color     [48]uint8
}

// Scan reads and decodes every unique input. It never deletes, writes, renames,
// or otherwise changes an input file. Inputs must contain JPEG, PNG, or GIF
// data; unsupported formats are visible failures that make the scan incomplete.
func Scan(ctx context.Context, inputs []ImageInput, options Options) (Report, error) {
	report := Report{Counts: Counts{Requested: len(inputs)}}
	unique, err := validateAndNormalize(inputs)
	if err != nil {
		return report, err
	}
	report.Counts.Unique = len(unique)
	options, err = normalizeOptions(options)
	if err != nil {
		return report, err
	}

	scanned := make([]scannedImage, 0, len(unique))
	for index, input := range unique {
		if err := ctx.Err(); err != nil {
			for _, pending := range unique[index:] {
				report.Failures = append(report.Failures, Failure{ID: pending.ID, Path: pending.Path, Stage: "context", Message: err.Error()})
			}
			break
		}
		item, failure := scanOne(input)
		if failure != nil {
			report.Failures = append(report.Failures, *failure)
			continue
		}
		scanned = append(scanned, item)
	}

	sort.Slice(scanned, func(i, j int) bool { return lessImage(scanned[i].public, scanned[j].public) })
	report.Images = make([]Image, len(scanned))
	for i := range scanned {
		report.Images[i] = scanned[i].public
	}
	report.Counts.Scanned = len(scanned)
	report.Counts.Failed = len(report.Failures)
	report.ByteExactGroups = buildExactGroups(scanned, "original_bytes")
	report.PixelExactGroups = buildExactGroups(scanned, "decoded_pixels")
	report.PixelExactGroups = omitRepeatedExactGroups(report.PixelExactGroups, report.ByteExactGroups)
	if options.DHashThreshold >= 0 {
		report.PerceptualPairs, err = buildPerceptualPairs(ctx, scanned, options)
		if err != nil {
			return report, err
		}
	}
	report.Counts.ByteExactGroups = len(report.ByteExactGroups)
	report.Counts.PixelExactGroups = len(report.PixelExactGroups)
	report.Counts.PerceptualCandidates = len(report.PerceptualPairs)

	if len(report.Failures) > 0 {
		return report, &ScanError{Failed: len(report.Failures), Total: len(unique)}
	}
	return report, nil
}

func validateAndNormalize(inputs []ImageInput) ([]ImageInput, error) {
	byID := make(map[string]int, len(inputs))
	unique := make([]ImageInput, 0, len(inputs))
	for i, input := range inputs {
		if input.ID == "" {
			return nil, fmt.Errorf("input %d has an empty photo ID", i)
		}
		if input.Path == "" {
			return nil, fmt.Errorf("input %q has an empty path", input.ID)
		}
		absolute, err := filepath.Abs(input.Path)
		if err != nil {
			return nil, fmt.Errorf("resolve path for photo %q: %w", input.ID, err)
		}
		input.Path = filepath.Clean(absolute)
		if input.Filename == "" {
			input.Filename = filepath.Base(input.Path)
		}
		if previous, ok := byID[input.ID]; ok {
			if unique[previous].Path != input.Path {
				return nil, fmt.Errorf("photo ID %q maps to conflicting paths %q and %q", input.ID, unique[previous].Path, input.Path)
			}
			unique[previous] = mergeInput(unique[previous], input)
			continue
		}
		byID[input.ID] = len(unique)
		unique = append(unique, input)
	}
	return unique, nil
}

func mergeInput(a, b ImageInput) ImageInput {
	a.Favorite = a.Favorite || b.Favorite
	if resolution(b.Width, b.Height) > resolution(a.Width, a.Height) {
		a.Width, a.Height = b.Width, b.Height
	}
	if b.Filename != "" && (a.Filename == "" || b.Filename < a.Filename) {
		a.Filename = b.Filename
	}
	return a
}

func normalizeOptions(options Options) (Options, error) {
	if options.DHashThreshold < -1 || options.DHashThreshold > 256 {
		return Options{}, fmt.Errorf("dHash threshold must be between -1 and 256")
	}
	if options.DHashThreshold == 0 {
		options.DHashThreshold = defaultDHashThreshold
	}
	if options.AspectRatioTolerance < 0 || options.AspectRatioTolerance >= 1 {
		return Options{}, fmt.Errorf("aspect ratio tolerance must be at least 0 and less than 1")
	}
	if options.AspectRatioTolerance == 0 {
		options.AspectRatioTolerance = defaultAspectRatioTolerance
	}
	if options.ColorDistance < 0 || options.ColorDistance > 255 {
		return Options{}, fmt.Errorf("color distance must be between 0 and 255")
	}
	if options.ColorDistance == 0 {
		options.ColorDistance = defaultColorDistance
	}
	return options, nil
}

func scanOne(input ImageInput) (scannedImage, *Failure) {
	info, err := os.Stat(input.Path)
	if err != nil {
		return scannedImage{}, fail(input, "read", err)
	}
	if !info.Mode().IsRegular() {
		return scannedImage{}, fail(input, "read", errors.New("path is not a regular file"))
	}

	f, err := os.Open(input.Path)
	if err != nil {
		return scannedImage{}, fail(input, "read", err)
	}
	byteDigest := sha256.New()
	_, copyErr := io.Copy(byteDigest, f)
	closeErr := f.Close()
	if copyErr != nil {
		return scannedImage{}, fail(input, "read", copyErr)
	}
	if closeErr != nil {
		return scannedImage{}, fail(input, "read", closeErr)
	}
	var byteHash [sha256.Size]byte
	copy(byteHash[:], byteDigest.Sum(nil))

	decoded, err := decodeImage(input.Path)
	if err != nil {
		return scannedImage{}, fail(input, "decode", err)
	}
	bounds := decoded.Bounds()
	if bounds.Empty() {
		return scannedImage{}, fail(input, "decode", errors.New("image has empty bounds"))
	}
	pixelHash := hashPixels(decoded)
	dhash, colorSignature := perceptualFeatures(decoded)
	public := Image{
		ID: input.ID, Path: input.Path, Filename: input.Filename,
		Width: input.Width, Height: input.Height, Favorite: input.Favorite,
		ByteSize: info.Size(), DecodedWidth: bounds.Dx(), DecodedHeight: bounds.Dy(),
		OriginalSHA256: hex.EncodeToString(byteHash[:]), PixelSHA256: hex.EncodeToString(pixelHash[:]),
	}
	return scannedImage{public: public, byteHash: byteHash, pixelHash: pixelHash, dhash: dhash, color: colorSignature}, nil
}

func fail(input ImageInput, stage string, err error) *Failure {
	return &Failure{ID: input.ID, Path: input.Path, Stage: stage, Message: err.Error()}
}

func decodeImage(path string) (*image.NRGBA, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	decoded, format, decodeErr := image.Decode(f)
	closeErr := f.Close()
	if decodeErr == nil && closeErr != nil {
		return nil, closeErr
	}
	if decodeErr == nil {
		if format == "jpeg" {
			orientation, err := readJPEGOrientation(path)
			if err != nil {
				return nil, fmt.Errorf("read JPEG orientation: %w", err)
			}
			decoded = applyOrientation(decoded, orientation)
		}
		return normalizeNRGBA(decoded), nil
	}
	return nil, decodeErr
}

// normalizeNRGBA gives the rest of the scanner a single, canonical pixel
// representation. Keeping the original buffer when it already starts at (0,0)
// avoids an unnecessary copy for NRGBA images.
func normalizeNRGBA(source image.Image) *image.NRGBA {
	if nrgba, ok := source.(*image.NRGBA); ok && nrgba.Bounds().Min == (image.Point{}) {
		return nrgba
	}
	bounds := source.Bounds()
	result := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(result, result.Bounds(), source, bounds.Min, draw.Src)
	return result
}

func readJPEGOrientation(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var signature [2]byte
	if _, err := io.ReadFull(f, signature[:]); err != nil {
		return 0, err
	}
	if signature != [2]byte{0xff, 0xd8} {
		return 0, errors.New("invalid JPEG signature")
	}
	for {
		var prefix [1]byte
		if _, err := io.ReadFull(f, prefix[:]); err != nil {
			return 0, err
		}
		if prefix[0] != 0xff {
			return 0, errors.New("invalid JPEG marker")
		}
		var marker [1]byte
		for {
			if _, err := io.ReadFull(f, marker[:]); err != nil {
				return 0, err
			}
			if marker[0] != 0xff {
				break
			}
		}
		if marker[0] == 0xd9 || marker[0] == 0xda {
			return 1, nil
		}
		if marker[0] == 0x00 || marker[0] == 0x01 || (marker[0] >= 0xd0 && marker[0] <= 0xd7) {
			continue
		}
		var lengthBytes [2]byte
		if _, err := io.ReadFull(f, lengthBytes[:]); err != nil {
			return 0, err
		}
		length := int(binary.BigEndian.Uint16(lengthBytes[:]))
		if length < 2 {
			return 0, errors.New("invalid JPEG segment length")
		}
		payloadLength := length - 2
		if marker[0] != 0xe1 {
			if _, err := io.CopyN(io.Discard, f, int64(payloadLength)); err != nil {
				return 0, err
			}
			continue
		}
		payload := make([]byte, payloadLength)
		if _, err := io.ReadFull(f, payload); err != nil {
			return 0, err
		}
		if !bytes.HasPrefix(payload, []byte("Exif\x00\x00")) {
			continue
		}
		return parseEXIFOrientation(payload[6:])
	}
}

func parseEXIFOrientation(tiff []byte) (int, error) {
	if len(tiff) < 8 {
		return 0, errors.New("truncated EXIF TIFF header")
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0, errors.New("invalid EXIF byte order")
	}
	if order.Uint16(tiff[2:4]) != 42 {
		return 0, errors.New("invalid EXIF TIFF marker")
	}
	offset := uint64(order.Uint32(tiff[4:8]))
	if offset+2 > uint64(len(tiff)) {
		return 0, errors.New("EXIF IFD offset is outside the segment")
	}
	entryCount := uint64(order.Uint16(tiff[offset : offset+2]))
	entriesStart := offset + 2
	if entryCount > (uint64(len(tiff))-entriesStart)/12 {
		return 0, errors.New("truncated EXIF IFD")
	}
	for index := uint64(0); index < entryCount; index++ {
		entry := tiff[entriesStart+index*12 : entriesStart+(index+1)*12]
		if order.Uint16(entry[0:2]) != 0x0112 {
			continue
		}
		if order.Uint16(entry[2:4]) != 3 || order.Uint32(entry[4:8]) != 1 {
			return 0, errors.New("invalid EXIF orientation field")
		}
		orientation := int(order.Uint16(entry[8:10]))
		if orientation < 1 || orientation > 8 {
			return 0, fmt.Errorf("invalid EXIF orientation value %d", orientation)
		}
		return orientation, nil
	}
	return 1, nil
}

func applyOrientation(source image.Image, orientation int) image.Image {
	if orientation == 1 {
		return source
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	outputWidth, outputHeight := width, height
	if orientation >= 5 {
		outputWidth, outputHeight = height, width
	}
	return orientedImage{source: source, orientation: orientation, bounds: image.Rect(0, 0, outputWidth, outputHeight)}
}

type orientedImage struct {
	source      image.Image
	orientation int
	bounds      image.Rectangle
}

func (o orientedImage) ColorModel() color.Model { return o.source.ColorModel() }
func (o orientedImage) Bounds() image.Rectangle { return o.bounds }

func (o orientedImage) At(x, y int) color.Color {
	sourceBounds := o.source.Bounds()
	width, height := sourceBounds.Dx(), sourceBounds.Dy()
	var sourceX, sourceY int
	switch o.orientation {
	case 2:
		sourceX, sourceY = width-1-x, y
	case 3:
		sourceX, sourceY = width-1-x, height-1-y
	case 4:
		sourceX, sourceY = x, height-1-y
	case 5:
		sourceX, sourceY = y, x
	case 6:
		sourceX, sourceY = y, height-1-x
	case 7:
		sourceX, sourceY = width-1-y, height-1-x
	case 8:
		sourceX, sourceY = width-1-y, x
	default:
		sourceX, sourceY = x, y
	}
	return o.source.At(sourceBounds.Min.X+sourceX, sourceBounds.Min.Y+sourceY)
}

func hashPixels(img *image.NRGBA) [sha256.Size]byte {
	digest := sha256.New()
	bounds := img.Bounds()
	writeDimension(digest, bounds.Dx())
	writeDimension(digest, bounds.Dy())
	if img.Stride == bounds.Dx()*4 {
		start := img.PixOffset(bounds.Min.X, bounds.Min.Y)
		_, _ = digest.Write(img.Pix[start : start+bounds.Dx()*bounds.Dy()*4])
		var result [sha256.Size]byte
		copy(result[:], digest.Sum(nil))
		return result
	}
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		start := img.PixOffset(bounds.Min.X, y)
		_, _ = digest.Write(img.Pix[start : start+bounds.Dx()*4])
	}
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result
}

func writeDimension(digest hash.Hash, value int) {
	var buffer [8]byte
	binary.LittleEndian.PutUint64(buffer[:], uint64(value))
	_, _ = digest.Write(buffer[:])
}

func perceptualFeatures(img *image.NRGBA) ([32]byte, [48]uint8) {
	thumbnail := areaDownsample(img, 17, 16)
	var result [32]byte
	for y := 0; y < 16; y++ {
		previous := luminance(thumbnail.NRGBAAt(0, y))
		for x := 0; x < 16; x++ {
			next := luminance(thumbnail.NRGBAAt(x+1, y))
			if previous > next {
				bit := y*16 + x
				result[bit/8] |= 1 << uint(7-bit%8)
			}
			previous = next
		}
	}
	colorThumbnail := areaDownsample(thumbnail, 4, 4)
	var colors [48]uint8
	index := 0
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			pixel := colorThumbnail.NRGBAAt(x, y)
			colors[index], colors[index+1], colors[index+2] = pixel.R, pixel.G, pixel.B
			index += 3
		}
	}
	return result, colors
}

// areaDownsample computes the exact box-filter average for every destination
// cell. Source pixels crossing a cell boundary contribute proportionally to
// both cells, which avoids texture aliasing when two copies have different
// dimensions.
func areaDownsample(img *image.NRGBA, width, height int) *image.NRGBA {
	bounds := img.Bounds()
	result := image.NewNRGBA(image.Rect(0, 0, width, height))
	sourceWidth, sourceHeight := int64(bounds.Dx()), int64(bounds.Dy())
	targetWidth, targetHeight := int64(width), int64(height)
	xSamples := downsampleAxis(sourceWidth, targetWidth)
	ySamples := downsampleAxis(sourceHeight, targetHeight)
	processRow := func(targetY int64) {
		for targetX := int64(0); targetX < targetWidth; targetX++ {
			var red, green, blue, totalWeight uint64
			for _, sourceY := range ySamples[targetY] {
				pixelOffset := img.PixOffset(bounds.Min.X+int(xSamples[targetX][0].position), bounds.Min.Y+int(sourceY.position))
				for _, sourceX := range xSamples[targetX] {
					weight := sourceX.weight * sourceY.weight
					alpha := img.Pix[pixelOffset+3]
					if alpha == 255 {
						red += uint64(img.Pix[pixelOffset]) * weight
						green += uint64(img.Pix[pixelOffset+1]) * weight
						blue += uint64(img.Pix[pixelOffset+2]) * weight
					} else {
						a := uint32(alpha)
						red += uint64((uint32(img.Pix[pixelOffset])*a+255*(255-a))/255) * weight
						green += uint64((uint32(img.Pix[pixelOffset+1])*a+255*(255-a))/255) * weight
						blue += uint64((uint32(img.Pix[pixelOffset+2])*a+255*(255-a))/255) * weight
					}
					totalWeight += weight
					pixelOffset += 4
				}
			}
			result.SetNRGBA(int(targetX), int(targetY), color.NRGBA{
				R: uint8((red + totalWeight/2) / totalWeight),
				G: uint8((green + totalWeight/2) / totalWeight),
				B: uint8((blue + totalWeight/2) / totalWeight),
				A: 255,
			})
		}
	}
	// Destination rows are independent. Parallelizing only large images keeps
	// small scans simple while letting one high-resolution scan use the host.
	if sourceWidth*sourceHeight >= 1<<20 && targetHeight > 1 && runtime.GOMAXPROCS(0) > 1 {
		workers := runtime.GOMAXPROCS(0)
		if workers > int(targetHeight) {
			workers = int(targetHeight)
		}
		rows := make(chan int64)
		var group sync.WaitGroup
		group.Add(workers)
		for worker := 0; worker < workers; worker++ {
			go func() {
				defer group.Done()
				for targetY := range rows {
					processRow(targetY)
				}
			}()
		}
		for targetY := int64(0); targetY < targetHeight; targetY++ {
			rows <- targetY
		}
		close(rows)
		group.Wait()
		return result
	}
	for targetY := int64(0); targetY < targetHeight; targetY++ {
		processRow(targetY)
	}
	return result
}

type weightedSource struct {
	position int64
	weight   uint64
}

// downsampleAxis precomputes the same overlap weights the former inner loop
// calculated per pixel. The values remain integers, so it preserves the exact
// box-filter arithmetic.
func downsampleAxis(sourceSize, targetSize int64) [][]weightedSource {
	result := make([][]weightedSource, targetSize)
	for target := int64(0); target < targetSize; target++ {
		start, end := target*sourceSize, (target+1)*sourceSize
		sourceStart := start / targetSize
		sourceEnd := (end + targetSize - 1) / targetSize
		result[target] = make([]weightedSource, 0, sourceEnd-sourceStart)
		for source := sourceStart; source < sourceEnd; source++ {
			pixelStart := source * targetSize
			weight := minInt64(end, pixelStart+targetSize) - maxInt64(start, pixelStart)
			result[target] = append(result[target], weightedSource{position: source, weight: uint64(weight)})
		}
	}
	return result
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func visibleNRGBA(value color.Color) color.NRGBA {
	pixel := color.NRGBAModel.Convert(value).(color.NRGBA)
	if pixel.A == 255 {
		return pixel
	}
	a := uint32(pixel.A)
	pixel.R = uint8((uint32(pixel.R)*a + 255*(255-a)) / 255)
	pixel.G = uint8((uint32(pixel.G)*a + 255*(255-a)) / 255)
	pixel.B = uint8((uint32(pixel.B)*a + 255*(255-a)) / 255)
	pixel.A = 255
	return pixel
}

func luminance(value color.Color) uint32 {
	pixel := visibleNRGBA(value)
	return 299*uint32(pixel.R) + 587*uint32(pixel.G) + 114*uint32(pixel.B)
}

func buildExactGroups(images []scannedImage, kind string) []ExactGroup {
	groups := make(map[[sha256.Size]byte][]Image)
	for _, item := range images {
		key := item.byteHash
		if kind == "decoded_pixels" {
			key = item.pixelHash
		}
		groups[key] = append(groups[key], item.public)
	}
	result := make([]ExactGroup, 0)
	for digest, members := range groups {
		if len(members) < 2 {
			continue
		}
		sort.Slice(members, func(i, j int) bool { return lessImage(members[i], members[j]) })
		result = append(result, ExactGroup{Kind: kind, Images: members, KeeperID: chooseKeeper(members).ID, Evidence: hex.EncodeToString(digest[:])})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].KeeperID == result[j].KeeperID {
			return result[i].Evidence < result[j].Evidence
		}
		return result[i].KeeperID < result[j].KeeperID
	})
	return result
}

func omitRepeatedExactGroups(pixelGroups, byteGroups []ExactGroup) []ExactGroup {
	byteMembership := make(map[string]struct{}, len(byteGroups))
	for _, group := range byteGroups {
		byteMembership[exactMembershipKey(group.Images)] = struct{}{}
	}
	result := pixelGroups[:0]
	for _, group := range pixelGroups {
		if _, repeated := byteMembership[exactMembershipKey(group.Images)]; repeated {
			continue
		}
		result = append(result, group)
	}
	return result
}

func exactMembershipKey(images []Image) string {
	var result strings.Builder
	for _, image := range images {
		result.WriteString(fmt.Sprintf("%d:%s", len(image.ID), image.ID))
	}
	return result.String()
}

func buildPerceptualPairs(ctx context.Context, images []scannedImage, options Options) ([]PerceptualPair, error) {
	result := make([]PerceptualPair, 0)
	for i := 0; i < len(images); i++ {
		for j := i + 1; j < len(images); j++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if images[i].pixelHash == images[j].pixelHash {
				continue
			}
			aspectDelta := aspectRatioDelta(images[i].public, images[j].public)
			if aspectDelta > options.AspectRatioTolerance {
				continue
			}
			distance := hammingDistance(images[i].dhash, images[j].dhash)
			if distance > options.DHashThreshold {
				continue
			}
			colorDistance := signatureDistance(images[i].color, images[j].color)
			if colorDistance > options.ColorDistance {
				continue
			}
			members := []Image{images[i].public, images[j].public}
			result = append(result, PerceptualPair{
				Left: members[0], Right: members[1], SuggestedKeeperID: chooseKeeper(members).ID,
				DHashDistance: distance, AspectRatioDelta: aspectDelta, ColorDistance: colorDistance,
				Classification: "candidate_for_visual_review", SafeForAutoDeletion: false,
			})
		}
	}
	return result, nil
}

func hammingDistance(a, b [32]byte) int {
	distance := 0
	for i := range a {
		distance += bits.OnesCount8(a[i] ^ b[i])
	}
	return distance
}

func signatureDistance(a, b [48]uint8) float64 {
	var total int
	for i := range a {
		total += absInt(int(a[i]) - int(b[i]))
	}
	return float64(total) / float64(len(a))
}

func aspectRatioDelta(a, b Image) float64 {
	ratioA := float64(a.DecodedWidth) / float64(a.DecodedHeight)
	ratioB := float64(b.DecodedWidth) / float64(b.DecodedHeight)
	return math.Abs(ratioA-ratioB) / math.Max(ratioA, ratioB)
}

func chooseKeeper(images []Image) Image {
	keeper := images[0]
	for _, candidate := range images[1:] {
		if betterKeeper(candidate, keeper) {
			keeper = candidate
		}
	}
	return keeper
}

func betterKeeper(candidate, current Image) bool {
	candidateResolution := originalResolution(candidate)
	currentResolution := originalResolution(current)
	if candidateResolution != currentResolution {
		return candidateResolution > currentResolution
	}
	if candidate.Favorite != current.Favorite {
		return candidate.Favorite
	}
	return candidate.ID < current.ID
}

func originalResolution(value Image) int64 {
	if result := resolution(value.Width, value.Height); result > 0 {
		return result
	}
	return resolution(value.DecodedWidth, value.DecodedHeight)
}

func resolution(width, height int) int64 {
	if width <= 0 || height <= 0 {
		return 0
	}
	return int64(width) * int64(height)
}

func lessImage(a, b Image) bool {
	if a.ID == b.ID {
		return a.Path < b.Path
	}
	return a.ID < b.ID
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
