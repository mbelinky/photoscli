function codedError(code, message, details) {
  var error = new Error(message);
  error.photoscliCode = code;
  error.photoscliDetails = details || {};
  return error;
}

function albumSummary(album) {
  return { id: album.id(), name: album.name() };
}

function albumDetails(album) {
  var ids = album.mediaItems.id();
  return { id: album.id(), name: album.name(), photoCount: ids.length, photoIds: ids };
}

function albumByID(Photos, id) {
  var album = Photos.albums.byId(id);
  if (!album.exists()) {
    throw codedError("not_found", "album was not found", { albumId: id });
  }
  return album;
}

function photoByID(Photos, id) {
  var photo = Photos.mediaItems.byId(id);
  return photo.exists() ? photo : null;
}

function knownVideoFilename(filename) {
  var value = String(filename || "").toLowerCase();
  var dot = value.lastIndexOf(".");
  var extension = dot >= 0 ? value.slice(dot) : "";
  return [
    ".3g2", ".3gp", ".asf", ".avi", ".divx", ".dv", ".flv", ".hevc",
    ".m2t", ".m2ts", ".m4v", ".mkv", ".mod", ".mov", ".mp4", ".mpeg",
    ".mpg", ".mts", ".qt", ".tod", ".webm", ".wmv"
  ].indexOf(extension) >= 0;
}

function dispatch(operation, input) {
  var Photos = Application("com.apple.Photos");
  if (operation === "auth.probe") {
    return { albumCount: Photos.albums().length };
  }
  if (operation === "albums.list") {
    return Photos.albums().map(albumSummary);
  }
  if (operation === "albums.get") {
    return albumDetails(albumByID(Photos, input.id));
  }
  if (operation === "photos.find") {
    var found = [];
    input.ids.forEach(function (id) {
      var photo = photoByID(Photos, id);
      if (photo !== null) found.push(photo.id());
    });
    return found;
  }
  if (operation === "photos.export") {
    var requested = input.images.map(function (image) {
      var photo = photoByID(Photos, image.id);
      if (photo === null) throw codedError("photos_not_found", "photo was not found", { photoId: image.id });
      return { photo: photo, directory: image.directory };
    });
    return requested.map(function (item) {
      var photo = item.photo;
      var result = { id: photo.id(), filename: photo.filename(), width: photo.width(), height: photo.height(), favorite: photo.favorite() };
      if (knownVideoFilename(result.filename)) {
        result.skipped = true;
        result.skipReason = "known_video_filename_extension";
        return result;
      }
      Photos.export([photo], { to: Path(item.directory), usingOriginals: false });
      return result;
    });
  }
  if (operation === "albums.create") {
    var created = Photos.make({ new: "album", named: input.name });
    return albumDetails(created);
  }
  throw codedError("bridge_failed", "unknown bridge operation", { operation: operation });
}

function run(argv) {
  var operation = argv[0] || "";
  try {
    var input = JSON.parse(argv[1] || "{}");
    return JSON.stringify({ ok: true, result: dispatch(operation, input) });
  } catch (error) {
    var number = Number(error.number || 0);
    var message = String(error.message || error);
    var code = error.photoscliCode || "photos_error";
    if (number === -1743 || message.toLowerCase().indexOf("not authorized") >= 0) code = "automation_denied";
    if (number === -1712) code = "timeout";
    return JSON.stringify({
      ok: false,
      error: {
        code: code,
        message: message,
        details: Object.assign({ appleEventError: number, operation: operation }, error.photoscliDetails || {})
      }
    });
  }
}
