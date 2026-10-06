on containsAll(haystack, needles)
	repeat with needle in needles
		if haystack does not contain (contents of needle) then return false
	end repeat
	return true
end containsAll

on run argv
	if (count of argv) < 2 then error "album ID and at least one photo ID are required"
	set albumID to item 1 of argv
	set photoIDs to items 2 through -1 of argv

	tell application "Photos"
		set targetAlbum to album id albumID
	end tell

	set batchStart to 1
	repeat while batchStart <= (count of photoIDs)
		set batchEnd to batchStart + 9
		if batchEnd > (count of photoIDs) then set batchEnd to count of photoIDs
		set batchIDs to items batchStart through batchEnd of photoIDs
		set mediaList to {}
		tell application "Photos"
			repeat with photoID in batchIDs
				copy media item id (contents of photoID) to end of mediaList
			end repeat
		end tell

		repeat with attempt from 1 to 10
			tell application "Photos"
				add mediaList to targetAlbum
				set albumPhotoIDs to id of media items of targetAlbum
			end tell
			if my containsAll(albumPhotoIDs, batchIDs) then exit repeat
			delay 0.2
		end repeat
		set batchStart to batchEnd + 1
	end repeat
	return "ok"
end run
