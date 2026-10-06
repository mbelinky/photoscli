on run argv
	if (count of argv) < 3 then error "skip duplicate check, album ID, and at least one file path are required"
	set skipDuplicateCheckArgument to item 1 of argv
	if skipDuplicateCheckArgument is "true" then
		set skipDuplicateCheck to true
	else if skipDuplicateCheckArgument is "false" then
		set skipDuplicateCheck to false
	else
		error "skip duplicate check must be true or false"
	end if
	set albumID to item 2 of argv
	set filePaths to items 3 through -1 of argv
	set fileList to {}
	repeat with filePath in filePaths
		copy POSIX file (contents of filePath) to end of fileList
	end repeat

	tell application "Photos"
		set importedItems to import fileList into album id (albumID) skip check duplicates skipDuplicateCheck
		if importedItems is missing value then return ""
		set importedIDs to {}
		repeat with importedItem in importedItems
			copy id of importedItem to end of importedIDs
		end repeat
	end tell

	set previousDelimiters to AppleScript's text item delimiters
	set AppleScript's text item delimiters to linefeed
	set output to importedIDs as text
	set AppleScript's text item delimiters to previousDelimiters
	return output
end run
