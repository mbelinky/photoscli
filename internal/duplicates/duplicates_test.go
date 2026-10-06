package duplicates

import (
	"context"
	"crypto/sha256"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestScanSeparatesByteAndPixelExactGroupsAndChoosesKeeper(t *testing.T) {
	directory := t.TempDir()
	source := testPattern(80, 60)
	first := filepath.Join(directory, "first.png")
	copyPath := filepath.Join(directory, "copy.png")
	reencoded := filepath.Join(directory, "reencoded.png")
	writePNG(t, first, source, png.BestSpeed)
	contents, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	writePNG(t, reencoded, source, png.BestCompression)

	report, err := Scan(context.Background(), []ImageInput{
		{ID: "photo-a", Path: first, Width: 100, Height: 50},
		{ID: "photo-b", Path: copyPath, Width: 100, Height: 50, Favorite: true},
		{ID: "photo-c", Path: reencoded, Width: 200, Height: 100},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Scanned != 3 || report.Counts.Failed != 0 {
		t.Fatalf("unexpected scan counts: %#v", report.Counts)
	}
	if len(report.ByteExactGroups) != 1 {
		t.Fatalf("expected one byte-exact group, got %#v", report.ByteExactGroups)
	}
	byteGroup := report.ByteExactGroups[0]
	if got := imageIDs(byteGroup.Images); !reflect.DeepEqual(got, []string{"photo-a", "photo-b"}) {
		t.Fatalf("unexpected byte-exact members: %v", got)
	}
	if byteGroup.KeeperID != "photo-b" {
		t.Fatalf("favorite should break equal-resolution tie, got keeper %q", byteGroup.KeeperID)
	}
	if len(report.PixelExactGroups) != 1 {
		t.Fatalf("expected one pixel-exact group, got %#v", report.PixelExactGroups)
	}
	pixelGroup := report.PixelExactGroups[0]
	if got := imageIDs(pixelGroup.Images); !reflect.DeepEqual(got, []string{"photo-a", "photo-b", "photo-c"}) {
		t.Fatalf("unexpected pixel-exact members: %v", got)
	}
	if pixelGroup.KeeperID != "photo-c" {
		t.Fatalf("higher original resolution should win before favorite, got keeper %q", pixelGroup.KeeperID)
	}
	if len(report.PerceptualPairs) != 0 {
		t.Fatalf("pixel-exact images should not also be review candidates: %#v", report.PerceptualPairs)
	}
}

func TestScanFindsRecompressedResizedCandidateButRejectsDistinctImage(t *testing.T) {
	directory := t.TempDir()
	large := filepath.Join(directory, "large.jpg")
	small := filepath.Join(directory, "small.jpg")
	distinct := filepath.Join(directory, "distinct.png")
	writeJPEG(t, large, testPattern(160, 120), 94)
	writeJPEG(t, small, testPattern(80, 62), 76)
	writePNG(t, distinct, solidImage(80, 60, color.NRGBA{R: 8, G: 30, B: 230, A: 255}), png.DefaultCompression)

	report, err := Scan(context.Background(), []ImageInput{
		{ID: "large", Path: large, Width: 160, Height: 120},
		{ID: "small", Path: small, Width: 80, Height: 62, Favorite: true},
		{ID: "different", Path: distinct},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.ByteExactGroups) != 0 || len(report.PixelExactGroups) != 0 {
		t.Fatalf("recompressed images must not be exact groups: %#v %#v", report.ByteExactGroups, report.PixelExactGroups)
	}
	if len(report.PerceptualPairs) != 1 {
		largeScan, _ := scanOne(ImageInput{ID: "large", Path: large})
		smallScan, _ := scanOne(ImageInput{ID: "small", Path: small})
		t.Fatalf("expected one review candidate, got %#v (dhash=%d color=%.2f aspect=%.4f)", report.PerceptualPairs,
			hammingDistance(largeScan.dhash, smallScan.dhash), signatureDistance(largeScan.color, smallScan.color),
			aspectRatioDelta(largeScan.public, smallScan.public))
	}
	pair := report.PerceptualPairs[0]
	if pair.Left.ID != "large" || pair.Right.ID != "small" {
		t.Fatalf("unexpected candidate: %#v", pair)
	}
	if pair.Classification != "candidate_for_visual_review" || pair.SafeForAutoDeletion {
		t.Fatalf("candidate safety classification was weakened: %#v", pair)
	}
	if pair.SuggestedKeeperID != "large" {
		t.Fatalf("higher resolution should be suggested keeper, got %q", pair.SuggestedKeeperID)
	}
}

func TestAreaDownsampleFindsTexturedResizedRecompressedCandidate(t *testing.T) {
	directory := t.TempDir()
	source := texturedPattern(511, 383)
	large := filepath.Join(directory, "textured-large.jpg")
	small := filepath.Join(directory, "textured-small.jpg")
	writeJPEG(t, large, source, 98)
	writeJPEG(t, small, areaDownsample(normalizeNRGBA(source), 143, 109), 74)

	largeDecoded, err := decodeImage(large)
	if err != nil {
		t.Fatal(err)
	}
	smallDecoded, err := decodeImage(small)
	if err != nil {
		t.Fatal(err)
	}
	oldDistance := hammingDistance(sparseDifferenceHash(largeDecoded), sparseDifferenceHash(smallDecoded))
	if oldDistance <= defaultDHashThreshold {
		t.Fatalf("fixture does not expose sparse-sampling aliasing: old dHash distance=%d", oldDistance)
	}

	report, err := Scan(context.Background(), []ImageInput{
		{ID: "textured-large", Path: large, Width: 511, Height: 383},
		{ID: "textured-small", Path: small, Width: 143, Height: 109},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.PerceptualPairs) != 1 {
		t.Fatalf("area averaging should recover textured resized copy; old distance=%d report=%#v", oldDistance, report)
	}
	if report.PerceptualPairs[0].DHashDistance > defaultDHashThreshold {
		t.Fatalf("area-averaged dHash remained too distant: %#v", report.PerceptualPairs[0])
	}
}

func TestScanNormalizesJPEGEXIFOrientationBeforeComparison(t *testing.T) {
	directory := t.TempDir()
	source := testPattern(80, 60)
	tagged := filepath.Join(directory, "orientation-six.jpg")
	upright := filepath.Join(directory, "upright.jpg")
	writeJPEG(t, tagged, source, 95)
	addOrientationSix(t, tagged)
	writeJPEG(t, upright, applyOrientation(source, 6), 95)

	report, err := Scan(context.Background(), []ImageInput{
		{ID: "tagged", Path: tagged},
		{ID: "upright", Path: upright},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Images) != 2 {
		t.Fatalf("expected two scanned images, got %#v", report.Images)
	}
	for _, scanned := range report.Images {
		if scanned.DecodedWidth != 60 || scanned.DecodedHeight != 80 {
			t.Fatalf("EXIF orientation was not reflected in decoded dimensions: %#v", scanned)
		}
	}
	if len(report.PerceptualPairs) != 1 {
		t.Fatalf("visually equivalent rotated encodings should be a review pair: %#v", report.PerceptualPairs)
	}
	if report.PerceptualPairs[0].DHashDistance > 12 {
		t.Fatalf("orientation-normalized comparison was too distant: %#v", report.PerceptualPairs[0])
	}
}

func TestNormalizedNRGBAMatchesPerPixelReferenceFixtures(t *testing.T) {
	directory := t.TempDir()
	patternPNG := filepath.Join(directory, "pattern.png")
	patternJPEG := filepath.Join(directory, "pattern.jpg")
	texturedJPEG := filepath.Join(directory, "textured.jpg")
	rotatedJPEG := filepath.Join(directory, "rotated.jpg")
	alphaPNG := filepath.Join(directory, "alpha.png")
	writePNG(t, patternPNG, testPattern(80, 60), png.DefaultCompression)
	writeJPEG(t, patternJPEG, testPattern(80, 60), 91)
	writeJPEG(t, texturedJPEG, texturedPattern(143, 109), 88)
	writeJPEG(t, rotatedJPEG, testPattern(80, 60), 91)
	addOrientationSix(t, rotatedJPEG)
	alpha := image.NewNRGBA(image.Rect(0, 0, 73, 41))
	for y := 0; y < 41; y++ {
		for x := 0; x < 73; x++ {
			alpha.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 3), G: uint8(y * 6), B: uint8(x + y), A: uint8((x*17 + y*11) % 256)})
		}
	}
	writePNG(t, alphaPNG, alpha, png.DefaultCompression)

	for _, path := range []string{patternPNG, patternJPEG, texturedJPEG, rotatedJPEG, alphaPNG} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			reference, err := decodeImageReference(path)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := decodeImage(path)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := hashPixels(actual), hashPixelsReference(reference); got != want {
				t.Fatalf("pixel hash changed: got %x want %x", got, want)
			}
			gotHash, gotColor := perceptualFeatures(actual)
			wantHash, wantColor := perceptualFeaturesReference(reference)
			if gotHash != wantHash || gotColor != wantColor {
				t.Fatalf("perceptual features changed: dHash got %x want %x color got %v want %v", gotHash, wantHash, gotColor, wantColor)
			}
		})
	}
}

func BenchmarkScanOne(b *testing.B) {
	path := filepath.Join(b.TempDir(), "synthetic-3000x2000.jpg")
	writeBenchmarkJPEG(b, path, 3000, 2000)
	input := ImageInput{ID: "synthetic", Path: path}
	b.ResetTimer()
	b.Run("old_per_pixel", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			legacyScanOne(b, path)
		}
	})
	b.Run("normalized_nrgba", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, failure := scanOne(input); failure != nil {
				b.Fatal(failure)
			}
		}
	})
}

func TestScanReturnsPartialReportAndErrorForMissingImage(t *testing.T) {
	directory := t.TempDir()
	valid := filepath.Join(directory, "valid.png")
	writePNG(t, valid, testPattern(16, 12), png.DefaultCompression)

	report, err := Scan(context.Background(), []ImageInput{
		{ID: "valid", Path: valid},
		{ID: "missing", Path: filepath.Join(directory, "missing.png")},
	}, Options{})
	var scanErr *ScanError
	if !errors.As(err, &scanErr) {
		t.Fatalf("expected ScanError, got %v", err)
	}
	if report.Counts.Requested != 2 || report.Counts.Unique != 2 || report.Counts.Scanned != 1 || report.Counts.Failed != 1 {
		t.Fatalf("unexpected partial counts: %#v", report.Counts)
	}
	if len(report.Failures) != 1 || report.Failures[0].ID != "missing" || report.Failures[0].Stage != "read" {
		t.Fatalf("missing input was not visible in report: %#v", report.Failures)
	}
}

func TestScanRejectsDuplicateIDWithConflictingPaths(t *testing.T) {
	directory := t.TempDir()
	first := filepath.Join(directory, "first.png")
	second := filepath.Join(directory, "second.png")
	writePNG(t, first, testPattern(8, 8), png.DefaultCompression)
	writePNG(t, second, testPattern(8, 8), png.DefaultCompression)

	_, err := Scan(context.Background(), []ImageInput{
		{ID: "same-id", Path: first},
		{ID: "same-id", Path: second},
	}, Options{})
	if err == nil {
		t.Fatal("expected conflicting-path validation error")
	}
}

func TestScanUsesIDAsFinalDeterministicKeeperTieBreak(t *testing.T) {
	directory := t.TempDir()
	imagePath := filepath.Join(directory, "source.png")
	writePNG(t, imagePath, testPattern(20, 15), png.DefaultCompression)
	contents, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	paths := make(map[string]string)
	for _, id := range []string{"z-photo", "a-photo", "m-photo"} {
		path := filepath.Join(directory, id+".png")
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		paths[id] = path
	}

	report, err := Scan(context.Background(), []ImageInput{
		{ID: "z-photo", Path: paths["z-photo"], Width: 20, Height: 15},
		{ID: "m-photo", Path: paths["m-photo"], Width: 20, Height: 15},
		{ID: "a-photo", Path: paths["a-photo"], Width: 20, Height: 15},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := report.ByteExactGroups[0].KeeperID; got != "a-photo" {
		t.Fatalf("expected lexically first ID as final tie-break, got %q", got)
	}
	if len(report.PixelExactGroups) != 0 {
		t.Fatalf("identical byte and pixel memberships should be reported once: %#v", report.PixelExactGroups)
	}
}

func TestPathMetacharactersAreHandledLiterally(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "$(touch${IFS}anything)")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "valid.png")
	writePNG(t, path, testPattern(12, 8), png.DefaultCompression)

	report, err := Scan(context.Background(), []ImageInput{{ID: "unsafe-name", Path: path}}, Options{})
	if err != nil || report.Counts.Scanned != 1 {
		t.Fatalf("literal path should scan normally, err=%v report=%#v", err, report)
	}
}

func TestUnsupportedHEICIsVisibleButJPEGContentWithHEICNameScans(t *testing.T) {
	directory := t.TempDir()
	rendered := filepath.Join(directory, "rendered.HEIC")
	writeJPEG(t, rendered, testPattern(32, 24), 90)

	report, err := Scan(context.Background(), []ImageInput{{ID: "rendered", Path: rendered}}, Options{})
	if err != nil || report.Counts.Scanned != 1 || report.Images[0].DecodedWidth != 32 {
		t.Fatalf("content detection should accept rendered JPEG bytes regardless of extension: err=%v report=%#v", err, report)
	}

	native := filepath.Join(directory, "native.heic")
	if err := os.WriteFile(native, []byte("unsupported HEIC container"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err = Scan(context.Background(), []ImageInput{{ID: "native", Path: native}}, Options{})
	var scanErr *ScanError
	if !errors.As(err, &scanErr) || report.Counts.Failed != 1 || len(report.Failures) != 1 || report.Failures[0].Stage != "decode" {
		t.Fatalf("unsupported HEIC must make the scan visibly incomplete: err=%v report=%#v", err, report)
	}
}

func imageIDs(images []Image) []string {
	result := make([]string, len(images))
	for i := range images {
		result[i] = images[i].ID
	}
	return result
}

func writePNG(t *testing.T, path string, value image.Image, compression png.CompressionLevel) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	encoder := png.Encoder{CompressionLevel: compression}
	encodeErr := encoder.Encode(f, value)
	closeErr := f.Close()
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}

func writeJPEG(t *testing.T, path string, value image.Image, quality int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	encodeErr := jpeg.Encode(f, value, &jpeg.Options{Quality: quality})
	closeErr := f.Close()
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}

func addOrientationSix(t *testing.T, path string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) < 2 || contents[0] != 0xff || contents[1] != 0xd8 {
		t.Fatal("test JPEG has no start-of-image marker")
	}
	app1 := []byte{
		0xff, 0xe1, 0x00, 0x22,
		'E', 'x', 'i', 'f', 0x00, 0x00,
		'I', 'I', 0x2a, 0x00, 0x08, 0x00, 0x00, 0x00,
		0x01, 0x00,
		0x12, 0x01, 0x03, 0x00, 0x01, 0x00, 0x00, 0x00, 0x06, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
	}
	withEXIF := make([]byte, 0, len(contents)+len(app1))
	withEXIF = append(withEXIF, contents[:2]...)
	withEXIF = append(withEXIF, app1...)
	withEXIF = append(withEXIF, contents[2:]...)
	if err := os.WriteFile(path, withEXIF, 0o600); err != nil {
		t.Fatal(err)
	}
}

func testPattern(width, height int) image.Image {
	result := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			result.SetNRGBA(x, y, color.NRGBA{
				R: uint8(20 + x*180/maxInt(1, width-1)),
				G: uint8(30 + y*170/maxInt(1, height-1)),
				B: uint8(40 + (x+y)*150/maxInt(1, width+height-2)),
				A: 255,
			})
		}
	}
	return result
}

func texturedPattern(width, height int) image.Image {
	result := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			base := 55 + x*130/maxInt(1, width-1) + y*20/maxInt(1, height-1)
			mixed := uint32(x)*73856093 ^ uint32(y)*19349663 ^ uint32(x*y)*83492791
			noise := int(mixed%141) - 70
			value := clampByte(base + noise)
			result.SetNRGBA(x, y, color.NRGBA{R: value, G: value, B: value, A: 255})
		}
	}
	return result
}

func sparseDifferenceHash(img image.Image) [32]byte {
	var result [32]byte
	for y := 0; y < 16; y++ {
		previous := luminance(sparseSampleAverage(img, 0, y, 17, 16))
		for x := 0; x < 16; x++ {
			next := luminance(sparseSampleAverage(img, x+1, y, 17, 16))
			if previous > next {
				bit := y*16 + x
				result[bit/8] |= 1 << uint(7-bit%8)
			}
			previous = next
		}
	}
	return result
}

func sparseSampleAverage(img image.Image, x, y, columns, rows int) color.NRGBA {
	bounds := img.Bounds()
	var red, green, blue uint32
	for sampleY := 0; sampleY < 3; sampleY++ {
		py := bounds.Min.Y + ((6*y+2*sampleY+1)*bounds.Dy())/(6*rows)
		if py >= bounds.Max.Y {
			py = bounds.Max.Y - 1
		}
		for sampleX := 0; sampleX < 3; sampleX++ {
			px := bounds.Min.X + ((6*x+2*sampleX+1)*bounds.Dx())/(6*columns)
			if px >= bounds.Max.X {
				px = bounds.Max.X - 1
			}
			pixel := visibleNRGBA(img.At(px, py))
			red += uint32(pixel.R)
			green += uint32(pixel.G)
			blue += uint32(pixel.B)
		}
	}
	return color.NRGBA{R: uint8(red / 9), G: uint8(green / 9), B: uint8(blue / 9), A: 255}
}

func clampByte(value int) uint8 {
	if value < 0 {
		return 0
	}
	if value > 255 {
		return 255
	}
	return uint8(value)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func solidImage(width, height int, value color.NRGBA) image.Image {
	result := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			result.SetNRGBA(x, y, value)
		}
	}
	return result
}

// The reference helpers intentionally preserve the scanner's former
// image.At-based implementation. They guard the normalized buffer against
// changing any hashes or perceptual evidence.
func decodeImageReference(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	decoded, format, decodeErr := image.Decode(f)
	closeErr := f.Close()
	if decodeErr != nil {
		return nil, decodeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if format != "jpeg" {
		return decoded, nil
	}
	orientation, err := readJPEGOrientation(path)
	if err != nil {
		return nil, err
	}
	return applyOrientation(decoded, orientation), nil
}

func hashPixelsReference(img image.Image) [sha256.Size]byte {
	digest := sha256.New()
	bounds := img.Bounds()
	writeDimension(digest, bounds.Dx())
	writeDimension(digest, bounds.Dy())
	row := make([]byte, bounds.Dx()*4)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x, offset := bounds.Min.X, 0; x < bounds.Max.X; x, offset = x+1, offset+4 {
			pixel := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			row[offset], row[offset+1], row[offset+2], row[offset+3] = pixel.R, pixel.G, pixel.B, pixel.A
		}
		_, _ = digest.Write(row)
	}
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result
}

func perceptualFeaturesReference(img image.Image) ([32]byte, [48]uint8) {
	thumbnail := areaDownsampleReference(img, 17, 16)
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
	colorThumbnail := areaDownsampleReference(thumbnail, 4, 4)
	var colors [48]uint8
	for y, index := 0, 0; y < 4; y++ {
		for x := 0; x < 4; x, index = x+1, index+3 {
			pixel := colorThumbnail.NRGBAAt(x, y)
			colors[index], colors[index+1], colors[index+2] = pixel.R, pixel.G, pixel.B
		}
	}
	return result, colors
}

func areaDownsampleReference(img image.Image, width, height int) *image.NRGBA {
	bounds := img.Bounds()
	result := image.NewNRGBA(image.Rect(0, 0, width, height))
	sourceWidth, sourceHeight := int64(bounds.Dx()), int64(bounds.Dy())
	targetWidth, targetHeight := int64(width), int64(height)
	for targetY := int64(0); targetY < targetHeight; targetY++ {
		yStart, yEnd := targetY*sourceHeight, (targetY+1)*sourceHeight
		sourceYStart, sourceYEnd := yStart/targetHeight, (yEnd+targetHeight-1)/targetHeight
		for targetX := int64(0); targetX < targetWidth; targetX++ {
			xStart, xEnd := targetX*sourceWidth, (targetX+1)*sourceWidth
			sourceXStart, sourceXEnd := xStart/targetWidth, (xEnd+targetWidth-1)/targetWidth
			var red, green, blue, totalWeight uint64
			for sourceY := sourceYStart; sourceY < sourceYEnd; sourceY++ {
				pixelYStart := sourceY * targetHeight
				yWeight := minInt64(yEnd, pixelYStart+targetHeight) - maxInt64(yStart, pixelYStart)
				for sourceX := sourceXStart; sourceX < sourceXEnd; sourceX++ {
					pixelXStart := sourceX * targetWidth
					weight := uint64((minInt64(xEnd, pixelXStart+targetWidth) - maxInt64(xStart, pixelXStart)) * yWeight)
					pixel := visibleNRGBA(img.At(bounds.Min.X+int(sourceX), bounds.Min.Y+int(sourceY)))
					red += uint64(pixel.R) * weight
					green += uint64(pixel.G) * weight
					blue += uint64(pixel.B) * weight
					totalWeight += weight
				}
			}
			result.SetNRGBA(int(targetX), int(targetY), color.NRGBA{
				R: uint8((red + totalWeight/2) / totalWeight), G: uint8((green + totalWeight/2) / totalWeight),
				B: uint8((blue + totalWeight/2) / totalWeight), A: 255,
			})
		}
	}
	return result
}

func legacyScanOne(b *testing.B, path string) {
	f, err := os.Open(path)
	if err != nil {
		b.Fatal(err)
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, f); err != nil {
		_ = f.Close()
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	decoded, err := decodeImageReference(path)
	if err != nil {
		b.Fatal(err)
	}
	_ = hashPixelsReference(decoded)
	_, _ = perceptualFeaturesReference(decoded)
}

func writeBenchmarkJPEG(b *testing.B, path string, width, height int) {
	b.Helper()
	result := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			result.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: uint8(x ^ y), A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	if err := jpeg.Encode(f, result, &jpeg.Options{Quality: 90}); err != nil {
		_ = f.Close()
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
}
