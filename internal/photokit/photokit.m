#import "photokit.h"

#import <Foundation/Foundation.h>
#import <Photos/Photos.h>
#import <errno.h>
#import <pthread.h>
#import <stdlib.h>
#import <string.h>
#import <time.h>

struct PKRequest {
    pthread_mutex_t mutex;
    pthread_cond_t condition;
    int references;
    int complete;
    char *resultJSON;
};

static PKRequest *PKRequestCreate(void) {
    PKRequest *request = calloc(1, sizeof(PKRequest));
    if (request == NULL) {
        return NULL;
    }
    if (pthread_mutex_init(&request->mutex, NULL) != 0) {
        free(request);
        return NULL;
    }
    if (pthread_cond_init(&request->condition, NULL) != 0) {
        pthread_mutex_destroy(&request->mutex);
        free(request);
        return NULL;
    }
    request->references = 1;
    return request;
}

static void PKRequestRetain(PKRequest *request) {
    pthread_mutex_lock(&request->mutex);
    request->references++;
    pthread_mutex_unlock(&request->mutex);
}

void PKRequestRelease(PKRequest *request) {
    if (request == NULL) {
        return;
    }
    int destroy = 0;
    pthread_mutex_lock(&request->mutex);
    request->references--;
    destroy = request->references == 0;
    pthread_mutex_unlock(&request->mutex);
    if (destroy) {
        pthread_cond_destroy(&request->condition);
        pthread_mutex_destroy(&request->mutex);
        free(request->resultJSON);
        free(request);
    }
}

static char *PKCopyJSON(NSDictionary *object) {
    NSError *error = nil;
    NSData *data = [NSJSONSerialization dataWithJSONObject:object options:0 error:&error];
    if (data == nil) {
        return strdup("{\"ok\":false,\"code\":\"bridge_error\",\"message\":\"could not encode PhotoKit result\"}");
    }
    NSString *json = [[NSString alloc] initWithData:data encoding:NSUTF8StringEncoding];
    return json == nil ? NULL : strdup(json.UTF8String);
}

static void PKRequestFinish(PKRequest *request, NSDictionary *result) {
    char *json = PKCopyJSON(result);
    if (json == NULL) {
        json = strdup("{\"ok\":false,\"code\":\"bridge_error\",\"message\":\"could not encode PhotoKit result\"}");
    }
    pthread_mutex_lock(&request->mutex);
    if (!request->complete) {
        request->resultJSON = json;
        request->complete = 1;
        pthread_cond_broadcast(&request->condition);
        json = NULL;
    }
    pthread_mutex_unlock(&request->mutex);
    free(json);
}

int PKRequestWait(PKRequest *request, int timeoutMilliseconds) {
    if (request == NULL || timeoutMilliseconds < 0) {
        return -1;
    }
    struct timespec deadline;
    if (clock_gettime(CLOCK_REALTIME, &deadline) != 0) {
        return -1;
    }
    deadline.tv_sec += timeoutMilliseconds / 1000;
    deadline.tv_nsec += (long)(timeoutMilliseconds % 1000) * 1000000L;
    if (deadline.tv_nsec >= 1000000000L) {
        deadline.tv_sec++;
        deadline.tv_nsec -= 1000000000L;
    }

    pthread_mutex_lock(&request->mutex);
    int status = 0;
    while (!request->complete && status == 0) {
        status = pthread_cond_timedwait(&request->condition, &request->mutex, &deadline);
    }
    int complete = request->complete;
    pthread_mutex_unlock(&request->mutex);
    if (complete) {
        return 1;
    }
    return status == ETIMEDOUT ? 0 : -1;
}

char *PKRequestCopyResultJSON(PKRequest *request) {
    if (request == NULL) {
        return NULL;
    }
    pthread_mutex_lock(&request->mutex);
    char *copy = request->complete && request->resultJSON != NULL
        ? strdup(request->resultJSON)
        : NULL;
    pthread_mutex_unlock(&request->mutex);
    return copy;
}

void PKFreeString(char *string) {
    free(string);
}

static NSString *PKAuthorizationStatusName(PHAuthorizationStatus status) {
    switch (status) {
        case PHAuthorizationStatusNotDetermined: return @"not_determined";
        case PHAuthorizationStatusRestricted: return @"restricted";
        case PHAuthorizationStatusDenied: return @"denied";
        case PHAuthorizationStatusAuthorized: return @"authorized";
        case PHAuthorizationStatusLimited: return @"limited";
    }
    return @"unknown";
}

static PHAuthorizationStatus PKReadAuthorizationStatus(void) {
    if (@available(macOS 11, *)) {
        return [PHPhotoLibrary authorizationStatusForAccessLevel:PHAccessLevelReadWrite];
    }
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
    return [PHPhotoLibrary authorizationStatus];
#pragma clang diagnostic pop
}

char *PKCopyAuthorizationStatusJSON(void) {
    @autoreleasepool {
        PHAuthorizationStatus status = PKReadAuthorizationStatus();
        return PKCopyJSON(@{
            @"ok": @YES,
            @"status": PKAuthorizationStatusName(status),
        });
    }
}

PKRequest *PKStartAuthorizationRequest(void) {
    PKRequest *request = PKRequestCreate();
    if (request == NULL) {
        return NULL;
    }
    PKRequestRetain(request);
    @try {
        void (^handler)(PHAuthorizationStatus) = ^(PHAuthorizationStatus status) {
            @autoreleasepool {
                PKRequestFinish(request, @{
                    @"ok": @YES,
                    @"status": PKAuthorizationStatusName(status),
                });
                PKRequestRelease(request);
            }
        };
        if (@available(macOS 11, *)) {
            [PHPhotoLibrary requestAuthorizationForAccessLevel:PHAccessLevelReadWrite handler:handler];
        } else {
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
            [PHPhotoLibrary requestAuthorization:handler];
#pragma clang diagnostic pop
        }
    } @catch (NSException *exception) {
        PKRequestFinish(request, @{
            @"ok": @NO,
            @"code": @"authorization_error",
            @"message": exception.reason ?: @"PhotoKit could not request authorization",
            @"status": @"unknown",
        });
        PKRequestRelease(request);
    }
    return request;
}

static NSMutableDictionary *PKBaseRemoveResult(NSString *albumID,
                                                NSArray<NSString *> *photoIDs,
                                                BOOL execute) {
    return [@{
        @"dryRun": @((BOOL)!execute),
        @"executed": @NO,
        @"verified": @NO,
        @"albumId": albumID,
        @"requestedPhotoIds": photoIDs,
        @"beforeMemberCount": @0,
    } mutableCopy];
}

char *PKCopyRemoveResultEncodingProbeJSON(int executeValue) {
    @autoreleasepool {
        return PKCopyJSON(PKBaseRemoveResult(@"encoding-probe", @[@"photo-probe"],
                                             executeValue != 0));
    }
}

static NSMutableDictionary *PKBaseSetHiddenResult(NSArray<NSString *> *photoIDs,
                                                   BOOL hidden,
                                                   BOOL execute) {
    return [@{
        @"dryRun": @((BOOL)!execute),
        @"executed": @NO,
        @"verified": @NO,
        @"hidden": @(hidden),
        @"requestedPhotoIds": photoIDs,
    } mutableCopy];
}

char *PKCopySetHiddenResultEncodingProbeJSON(int executeValue) {
    @autoreleasepool {
        return PKCopyJSON(PKBaseSetHiddenResult(@[@"photo-probe"], YES,
                                                executeValue != 0));
    }
}

static NSDictionary *PKFailure(NSMutableDictionary *result,
                               NSString *code,
                               NSString *message) {
    result[@"ok"] = @NO;
    result[@"code"] = code;
    result[@"message"] = message;
    return result;
}

static PHAssetCollection *PKFetchExactUserAlbum(NSString *albumID,
                                                 NSString **failureCode,
                                                 NSString **failureMessage) {
    PHFetchResult<PHAssetCollection *> *fetch =
        [PHAssetCollection fetchAssetCollectionsWithLocalIdentifiers:@[albumID] options:nil];
    if (fetch.count != 1) {
        *failureCode = @"album_not_found";
        *failureMessage = @"the exact album ID was not found";
        return nil;
    }
    PHAssetCollection *album = fetch.firstObject;
    if (![album.localIdentifier isEqualToString:albumID]) {
        *failureCode = @"album_mismatch";
        *failureMessage = @"PhotoKit returned a different album identifier";
        return nil;
    }
    if (album.assetCollectionType != PHAssetCollectionTypeAlbum) {
        *failureCode = @"not_user_album";
        *failureMessage = @"the identifier does not refer to a user album";
        return nil;
    }
    return album;
}

static NSArray<NSString *> *PKMissingIDs(NSArray<NSString *> *expected,
                                         NSSet<NSString *> *actual) {
    NSMutableArray<NSString *> *missing = [NSMutableArray array];
    for (NSString *identifier in expected) {
        if (![actual containsObject:identifier]) {
            [missing addObject:identifier];
        }
    }
    return missing;
}

static NSDictionary *PKVerifyRemoval(NSString *albumID,
                                     NSString *albumTitle,
                                     NSArray<NSString *> *requestedIDs,
                                     NSArray<NSString *> *priorOtherIDs,
                                     NSUInteger beforeCount) {
    NSMutableDictionary *result = PKBaseRemoveResult(albumID, requestedIDs, YES);
    result[@"executed"] = @YES;
    result[@"albumTitle"] = albumTitle;
    result[@"beforeMemberCount"] = @(beforeCount);

    NSString *failureCode = nil;
    NSString *failureMessage = nil;
    PHAssetCollection *album = PKFetchExactUserAlbum(albumID, &failureCode, &failureMessage);
    if (album == nil) {
        return PKFailure(result, @"verification_failed",
                         @"the album could not be read back after PhotoKit reported success");
    }

    PHFetchResult<PHAsset *> *members = [PHAsset fetchAssetsInAssetCollection:album options:nil];
    NSMutableSet<NSString *> *memberIDs = [NSMutableSet setWithCapacity:members.count];
    for (PHAsset *asset in members) {
        [memberIDs addObject:asset.localIdentifier];
    }
    result[@"afterMemberCount"] = @(members.count);

    NSMutableArray<NSString *> *stillMembers = [NSMutableArray array];
    for (NSString *identifier in requestedIDs) {
        if ([memberIDs containsObject:identifier]) {
            [stillMembers addObject:identifier];
        }
    }
    NSArray<NSString *> *lostOtherMembers = PKMissingIDs(priorOtherIDs, memberIDs);

    PHFetchResult<PHAsset *> *libraryFetch =
        [PHAsset fetchAssetsWithLocalIdentifiers:requestedIDs options:nil];
    NSMutableSet<NSString *> *libraryIDs = [NSMutableSet setWithCapacity:libraryFetch.count];
    for (PHAsset *asset in libraryFetch) {
        [libraryIDs addObject:asset.localIdentifier];
    }
    NSArray<NSString *> *missingFromLibrary = PKMissingIDs(requestedIDs, libraryIDs);

    if (stillMembers.count > 0 || lostOtherMembers.count > 0 || missingFromLibrary.count > 0) {
        result[@"stillInAlbumPhotoIds"] = stillMembers;
        result[@"missingPriorMemberPhotoIds"] = lostOtherMembers;
        result[@"missingFromLibraryPhotoIds"] = missingFromLibrary;
        return PKFailure(result, @"verification_failed",
                         @"PhotoKit reported success, but the album and library readback did not verify the requested membership-only removal");
    }

    result[@"ok"] = @YES;
    result[@"verified"] = @YES;
    result[@"removedPhotoIds"] = requestedIDs;
    return result;
}

PKRequest *PKStartRemoveFromAlbum(const char *albumIDBytes,
                                  const char *const *photoIDBytes,
                                  size_t photoIDCount,
                                  int executeValue) {
    PKRequest *request = PKRequestCreate();
    if (request == NULL) {
        return NULL;
    }

    @autoreleasepool {
        BOOL execute = executeValue != 0;
        NSString *albumID = albumIDBytes == NULL
            ? nil
            : [[NSString alloc] initWithUTF8String:albumIDBytes];
        NSMutableArray<NSString *> *photoIDs = [NSMutableArray arrayWithCapacity:photoIDCount];
        BOOL invalidUTF8 = albumID == nil;
        for (size_t index = 0; index < photoIDCount; index++) {
            NSString *identifier = photoIDBytes[index] == NULL
                ? nil
                : [[NSString alloc] initWithUTF8String:photoIDBytes[index]];
            if (identifier == nil) {
                invalidUTF8 = YES;
                break;
            }
            [photoIDs addObject:identifier];
        }
        if (invalidUTF8) {
            PKRequestFinish(request, @{
                @"ok": @NO,
                @"code": @"invalid_input",
                @"message": @"album and photo IDs must be valid UTF-8",
            });
            return request;
        }

        NSMutableDictionary *result = PKBaseRemoveResult(albumID, photoIDs, execute);
        BOOL retainedForCompletion = NO;
        @try {
            PHAuthorizationStatus authorization = PKReadAuthorizationStatus();
            if (authorization != PHAuthorizationStatusAuthorized) {
                result[@"authorizationStatus"] = PKAuthorizationStatusName(authorization);
                PKRequestFinish(request, PKFailure(result, @"authorization_required",
                    @"full Photos read/write authorization is required; call RequestAuthorization explicitly"));
                return request;
            }

            NSString *failureCode = nil;
            NSString *failureMessage = nil;
            PHAssetCollection *album = PKFetchExactUserAlbum(albumID, &failureCode, &failureMessage);
            if (album == nil) {
                PKRequestFinish(request, PKFailure(result, failureCode, failureMessage));
                return request;
            }
            result[@"albumTitle"] = album.localizedTitle ?: @"";
            if (![album canPerformEditOperation:PHCollectionEditOperationRemoveContent]) {
                PKRequestFinish(request, PKFailure(result, @"album_not_editable",
                    @"the user album does not allow removing content"));
                return request;
            }

            PHFetchResult<PHAsset *> *members =
                [PHAsset fetchAssetsInAssetCollection:album options:nil];
            result[@"beforeMemberCount"] = @(members.count);
            NSMutableDictionary<NSString *, PHAsset *> *memberByID =
                [NSMutableDictionary dictionaryWithCapacity:members.count];
            for (PHAsset *asset in members) {
                memberByID[asset.localIdentifier] = asset;
            }

            PHFetchResult<PHAsset *> *libraryFetch =
                [PHAsset fetchAssetsWithLocalIdentifiers:photoIDs options:nil];
            NSMutableSet<NSString *> *libraryIDs =
                [NSMutableSet setWithCapacity:libraryFetch.count];
            for (PHAsset *asset in libraryFetch) {
                [libraryIDs addObject:asset.localIdentifier];
            }
            NSArray<NSString *> *missingFromLibrary = PKMissingIDs(photoIDs, libraryIDs);
            if (missingFromLibrary.count > 0) {
                result[@"missingPhotoIds"] = missingFromLibrary;
                PKRequestFinish(request, PKFailure(result, @"photos_not_found",
                    @"one or more exact photo IDs were not found; nothing was changed"));
                return request;
            }

            NSMutableArray<NSString *> *notMembers = [NSMutableArray array];
            NSMutableArray<PHAsset *> *assetsToRemove =
                [NSMutableArray arrayWithCapacity:photoIDs.count];
            for (NSString *identifier in photoIDs) {
                PHAsset *asset = memberByID[identifier];
                if (asset == nil) {
                    [notMembers addObject:identifier];
                } else {
                    [assetsToRemove addObject:asset];
                }
            }
            if (notMembers.count > 0) {
                result[@"notAlbumMembersPhotoIds"] = notMembers;
                PKRequestFinish(request, PKFailure(result, @"photos_not_in_album",
                    @"one or more photos are not members of the exact album; nothing was changed"));
                return request;
            }

            NSMutableArray<NSString *> *priorOtherIDs =
                [NSMutableArray arrayWithCapacity:members.count - assetsToRemove.count];
            NSSet<NSString *> *requestedSet = [NSSet setWithArray:photoIDs];
            for (PHAsset *asset in members) {
                if (![requestedSet containsObject:asset.localIdentifier]) {
                    [priorOtherIDs addObject:asset.localIdentifier];
                }
            }

            if (!execute) {
                result[@"ok"] = @YES;
                result[@"wouldRemovePhotoIds"] = photoIDs;
                PKRequestFinish(request, result);
                return request;
            }

            result[@"executed"] = @YES;
            PKRequestRetain(request);
            retainedForCompletion = YES;
            __block NSString *changeException = nil;
            [[PHPhotoLibrary sharedPhotoLibrary]
                performChanges:^{
                    @try {
                        PHAssetCollectionChangeRequest *change =
                            [PHAssetCollectionChangeRequest changeRequestForAssetCollection:album assets:members];
                        if (change == nil) {
                            changeException = @"PhotoKit did not create an album change request";
                            return;
                        }
                        // This changes membership in the selected album only.
                        [change removeAssets:assetsToRemove];
                    } @catch (NSException *exception) {
                        changeException = exception.reason ?: @"PhotoKit rejected the album change request";
                    }
                }
                completionHandler:^(BOOL success, NSError *error) {
                    @autoreleasepool {
                        if (changeException != nil) {
                            PKRequestFinish(request, PKFailure(result, @"change_rejected", changeException));
                            PKRequestRelease(request);
                            return;
                        }
                        if (!success) {
                            NSString *message = error.localizedDescription ?: @"PhotoKit did not apply the album membership change";
                            PKRequestFinish(request, PKFailure(result, @"change_failed", message));
                            PKRequestRelease(request);
                            return;
                        }

                        dispatch_async(dispatch_get_global_queue(QOS_CLASS_USER_INITIATED, 0), ^{
                            @autoreleasepool {
                                NSDictionary *verification = nil;
                                for (NSUInteger attempt = 0; attempt < 10; attempt++) {
                                    verification = PKVerifyRemoval(albumID,
                                        album.localizedTitle ?: @"", photoIDs,
                                        priorOtherIDs, members.count);
                                    if ([verification[@"ok"] boolValue]) {
                                        break;
                                    }
                                    if (attempt < 9) {
                                        [NSThread sleepForTimeInterval:0.2];
                                    }
                                }
                                PKRequestFinish(request, verification);
                                PKRequestRelease(request);
                            }
                        });
                    }
                }];
            retainedForCompletion = NO;
        } @catch (NSException *exception) {
            PKRequestFinish(request, PKFailure(result, @"photokit_exception",
                exception.reason ?: @"PhotoKit rejected the request"));
            if (retainedForCompletion) {
                PKRequestRelease(request);
            }
        }
    }
    return request;
}

static PHFetchOptions *PKHiddenAssetFetchOptions(void) {
    PHFetchOptions *options = [[PHFetchOptions alloc] init];
    options.includeHiddenAssets = YES;
    return options;
}

// PKHiddenAssetsVisible reports whether PhotoKit returns hidden photos to
// this process. When Photos locks the Hidden album (Settings > Use Touch ID or
// password), hidden photos are invisible to apps, so a hide could be neither
// verified nor undone. A library with no hidden photos also reports NO.
static BOOL PKHiddenAssetsVisible(void) {
    PHFetchOptions *options = PKHiddenAssetFetchOptions();
    options.predicate = [NSPredicate predicateWithFormat:@"hidden == YES"];
    options.fetchLimit = 1;
    return [PHAsset fetchAssetsWithOptions:options].count > 0;
}

static NSDictionary *PKVerifySetHidden(NSArray<NSString *> *requestedIDs,
                                        BOOL hidden,
                                        NSArray<NSString *> *changedIDs,
                                        NSArray<NSString *> *alreadyIDs) {
    NSMutableDictionary *result = PKBaseSetHiddenResult(requestedIDs, hidden, YES);
    result[@"executed"] = @YES;
    result[@"alreadyInStatePhotoIds"] = alreadyIDs;
    PHFetchResult<PHAsset *> *fetch = [PHAsset fetchAssetsWithLocalIdentifiers:requestedIDs
                                                                         options:PKHiddenAssetFetchOptions()];
    NSMutableDictionary<NSString *, PHAsset *> *assets = [NSMutableDictionary dictionaryWithCapacity:fetch.count];
    for (PHAsset *asset in fetch) { assets[asset.localIdentifier] = asset; }
    NSMutableArray<NSString *> *missing = [NSMutableArray array];
    NSMutableArray<NSString *> *notInState = [NSMutableArray array];
    for (NSString *identifier in requestedIDs) {
        PHAsset *asset = assets[identifier];
        if (asset == nil) [missing addObject:identifier];
        else if (asset.hidden != hidden) [notInState addObject:identifier];
    }
    if (missing.count > 0 || notInState.count > 0) {
        result[@"missingFromLibraryPhotoIds"] = missing;
        result[@"notInStatePhotoIds"] = notInState;
        result[@"changedPhotoIds"] = changedIDs;
        return PKFailure(result, @"verification_failed",
                         @"PhotoKit reported success, but hidden-state readback did not verify every requested photo");
    }
    result[@"ok"] = @YES;
    result[@"verified"] = @YES;
    result[@"changedPhotoIds"] = changedIDs;
    return result;
}

PKRequest *PKStartSetHidden(const char *const *photoIDBytes,
                            size_t photoIDCount,
                            int hiddenValue,
                            int executeValue) {
    PKRequest *request = PKRequestCreate();
    if (request == NULL) return NULL;
    @autoreleasepool {
        BOOL hidden = hiddenValue != 0;
        BOOL execute = executeValue != 0;
        NSMutableArray<NSString *> *photoIDs = [NSMutableArray arrayWithCapacity:photoIDCount];
        BOOL invalidUTF8 = NO;
        for (size_t index = 0; index < photoIDCount; index++) {
            NSString *identifier = photoIDBytes[index] == NULL ? nil : [[NSString alloc] initWithUTF8String:photoIDBytes[index]];
            if (identifier == nil) { invalidUTF8 = YES; break; }
            [photoIDs addObject:identifier];
        }
        if (invalidUTF8) {
            PKRequestFinish(request, @{@"ok": @NO, @"code": @"invalid_input", @"message": @"photo IDs must be valid UTF-8"});
            return request;
        }
        NSMutableDictionary *result = PKBaseSetHiddenResult(photoIDs, hidden, execute);
        BOOL retainedForCompletion = NO;
        @try {
            PHAuthorizationStatus authorization = PKReadAuthorizationStatus();
            if (authorization != PHAuthorizationStatusAuthorized) {
                result[@"authorizationStatus"] = PKAuthorizationStatusName(authorization);
                PKRequestFinish(request, PKFailure(result, @"authorization_required", @"full Photos read/write authorization is required; call RequestAuthorization explicitly"));
                return request;
            }
            if (!PKHiddenAssetsVisible()) {
                result[@"hiddenAssetsVisible"] = @NO;
                PKRequestFinish(request, PKFailure(result, @"hidden_album_locked",
                    @"PhotoKit cannot see hidden photos, which happens when Photos locks the Hidden album (Photos Settings: Use Touch ID or password). A hide could not be verified or undone by photoscli, so nothing was changed."));
                return request;
            }
            PHFetchResult<PHAsset *> *fetch = [PHAsset fetchAssetsWithLocalIdentifiers:photoIDs options:PKHiddenAssetFetchOptions()];
            NSMutableDictionary<NSString *, PHAsset *> *assets = [NSMutableDictionary dictionaryWithCapacity:fetch.count];
            for (PHAsset *asset in fetch) { assets[asset.localIdentifier] = asset; }
            NSArray<NSString *> *missing = PKMissingIDs(photoIDs, [NSSet setWithArray:assets.allKeys]);
            if (missing.count > 0) {
                result[@"missingPhotoIds"] = missing;
                PKRequestFinish(request, PKFailure(result, @"photos_not_found", @"one or more exact photo IDs were not found; nothing was changed"));
                return request;
            }
            NSMutableArray<NSString *> *alreadyIDs = [NSMutableArray array];
            NSMutableArray<PHAsset *> *assetsToChange = [NSMutableArray array];
            NSMutableArray<NSString *> *changedIDs = [NSMutableArray array];
            for (NSString *identifier in photoIDs) {
                PHAsset *asset = assets[identifier];
                if (asset.hidden == hidden) [alreadyIDs addObject:identifier];
                else { [assetsToChange addObject:asset]; [changedIDs addObject:identifier]; }
            }
            result[@"alreadyInStatePhotoIds"] = alreadyIDs;
            if (!execute) {
                result[@"ok"] = @YES;
                result[@"wouldChangePhotoIds"] = changedIDs;
                PKRequestFinish(request, result);
                return request;
            }
            result[@"executed"] = @YES;
            if (assetsToChange.count == 0) {
                NSDictionary *verification = PKVerifySetHidden(photoIDs, hidden, changedIDs, alreadyIDs);
                PKRequestFinish(request, verification);
                return request;
            }
            PKRequestRetain(request);
            retainedForCompletion = YES;
            __block NSString *changeException = nil;
            [[PHPhotoLibrary sharedPhotoLibrary] performChanges:^{
                @try {
                    for (PHAsset *asset in assetsToChange) {
                        PHAssetChangeRequest *change = [PHAssetChangeRequest changeRequestForAsset:asset];
                        if (change == nil) { changeException = @"PhotoKit did not create an asset change request"; return; }
                        change.hidden = hidden;
                    }
                } @catch (NSException *exception) { changeException = exception.reason ?: @"PhotoKit rejected the hidden-state change request"; }
            } completionHandler:^(BOOL success, NSError *error) {
                @autoreleasepool {
                    if (changeException != nil) { PKRequestFinish(request, PKFailure(result, @"change_rejected", changeException)); PKRequestRelease(request); return; }
                    if (!success) { PKRequestFinish(request, PKFailure(result, @"change_failed", error.localizedDescription ?: @"PhotoKit did not apply the hidden-state change")); PKRequestRelease(request); return; }
                    dispatch_async(dispatch_get_global_queue(QOS_CLASS_USER_INITIATED, 0), ^{
                        @autoreleasepool {
                            NSDictionary *verification = nil;
                            for (NSUInteger attempt = 0; attempt < 10; attempt++) {
                                verification = PKVerifySetHidden(photoIDs, hidden, changedIDs, alreadyIDs);
                                if ([verification[@"ok"] boolValue]) break;
                                if (attempt < 9) [NSThread sleepForTimeInterval:0.2];
                            }
                            PKRequestFinish(request, verification);
                            PKRequestRelease(request);
                        }
                    });
                }
            }];
            retainedForCompletion = NO;
        } @catch (NSException *exception) {
            PKRequestFinish(request, PKFailure(result, @"photokit_exception", exception.reason ?: @"PhotoKit rejected the request"));
            if (retainedForCompletion) PKRequestRelease(request);
        }
    }
    return request;
}

static NSMutableDictionary *PKBaseSetDatesResult(NSArray<NSString *> *photoIDs, BOOL execute) {
    return [@{
        @"dryRun": @((BOOL)!execute),
        @"executed": @NO,
        @"verified": @NO,
        @"requestedPhotoIds": photoIDs,
    } mutableCopy];
}

// A creation date counts as set when it is within one second of the target;
// Photos stores dates with sub-second noise.
static BOOL PKDateMatches(NSDate *actual, NSDate *wanted) {
    return actual != nil && fabs([actual timeIntervalSinceDate:wanted]) < 1.0;
}

static NSDictionary *PKVerifySetDates(NSArray<NSString *> *requestedIDs,
                                      NSDictionary<NSString *, NSDate *> *wanted,
                                      NSArray<NSString *> *changedIDs,
                                      NSArray<NSString *> *alreadyIDs) {
    NSMutableDictionary *result = PKBaseSetDatesResult(requestedIDs, YES);
    result[@"executed"] = @YES;
    result[@"alreadyInStatePhotoIds"] = alreadyIDs;
    PHFetchResult<PHAsset *> *fetch = [PHAsset fetchAssetsWithLocalIdentifiers:requestedIDs options:nil];
    NSMutableDictionary<NSString *, PHAsset *> *assets = [NSMutableDictionary dictionaryWithCapacity:fetch.count];
    for (PHAsset *asset in fetch) { assets[asset.localIdentifier] = asset; }
    NSMutableArray<NSString *> *missing = [NSMutableArray array];
    NSMutableArray<NSString *> *notInState = [NSMutableArray array];
    for (NSString *identifier in requestedIDs) {
        PHAsset *asset = assets[identifier];
        if (asset == nil) [missing addObject:identifier];
        else if (!PKDateMatches(asset.creationDate, wanted[identifier])) [notInState addObject:identifier];
    }
    result[@"changedPhotoIds"] = changedIDs;
    if (missing.count > 0 || notInState.count > 0) {
        result[@"missingFromLibraryPhotoIds"] = missing;
        result[@"notInStatePhotoIds"] = notInState;
        return PKFailure(result, @"verification_failed",
                         @"PhotoKit reported success, but the creation-date readback did not verify every requested photo");
    }
    result[@"ok"] = @YES;
    result[@"verified"] = @YES;
    return result;
}

PKRequest *PKStartSetCreationDates(const char *const *photoIDBytes,
                                   const double *epochSeconds,
                                   size_t photoIDCount,
                                   int executeValue) {
    PKRequest *request = PKRequestCreate();
    if (request == NULL) return NULL;
    @autoreleasepool {
        BOOL execute = executeValue != 0;
        NSMutableArray<NSString *> *photoIDs = [NSMutableArray arrayWithCapacity:photoIDCount];
        NSMutableDictionary<NSString *, NSDate *> *wanted = [NSMutableDictionary dictionaryWithCapacity:photoIDCount];
        for (size_t index = 0; index < photoIDCount; index++) {
            NSString *identifier = photoIDBytes[index] == NULL ? nil : [[NSString alloc] initWithUTF8String:photoIDBytes[index]];
            if (identifier == nil) {
                PKRequestFinish(request, @{@"ok": @NO, @"code": @"invalid_input", @"message": @"photo IDs must be valid UTF-8"});
                return request;
            }
            [photoIDs addObject:identifier];
            wanted[identifier] = [NSDate dateWithTimeIntervalSince1970:epochSeconds[index]];
        }
        NSMutableDictionary *result = PKBaseSetDatesResult(photoIDs, execute);
        BOOL retainedForCompletion = NO;
        @try {
            PHAuthorizationStatus authorization = PKReadAuthorizationStatus();
            if (authorization != PHAuthorizationStatusAuthorized) {
                result[@"authorizationStatus"] = PKAuthorizationStatusName(authorization);
                PKRequestFinish(request, PKFailure(result, @"authorization_required", @"full Photos read/write authorization is required; call RequestAuthorization explicitly"));
                return request;
            }
            PHFetchResult<PHAsset *> *fetch = [PHAsset fetchAssetsWithLocalIdentifiers:photoIDs options:nil];
            NSMutableDictionary<NSString *, PHAsset *> *assets = [NSMutableDictionary dictionaryWithCapacity:fetch.count];
            for (PHAsset *asset in fetch) { assets[asset.localIdentifier] = asset; }
            NSArray<NSString *> *missing = PKMissingIDs(photoIDs, [NSSet setWithArray:assets.allKeys]);
            if (missing.count > 0) {
                result[@"missingPhotoIds"] = missing;
                PKRequestFinish(request, PKFailure(result, @"photos_not_found", @"one or more exact photo IDs were not found; nothing was changed"));
                return request;
            }
            NSMutableArray<NSString *> *alreadyIDs = [NSMutableArray array];
            NSMutableArray<PHAsset *> *assetsToChange = [NSMutableArray array];
            NSMutableArray<NSString *> *changedIDs = [NSMutableArray array];
            for (NSString *identifier in photoIDs) {
                PHAsset *asset = assets[identifier];
                if (PKDateMatches(asset.creationDate, wanted[identifier])) [alreadyIDs addObject:identifier];
                else { [assetsToChange addObject:asset]; [changedIDs addObject:identifier]; }
            }
            result[@"alreadyInStatePhotoIds"] = alreadyIDs;
            if (!execute) {
                result[@"ok"] = @YES;
                result[@"wouldChangePhotoIds"] = changedIDs;
                PKRequestFinish(request, result);
                return request;
            }
            result[@"executed"] = @YES;
            if (assetsToChange.count == 0) {
                PKRequestFinish(request, PKVerifySetDates(photoIDs, wanted, changedIDs, alreadyIDs));
                return request;
            }
            PKRequestRetain(request);
            retainedForCompletion = YES;
            __block NSString *changeException = nil;
            [[PHPhotoLibrary sharedPhotoLibrary] performChanges:^{
                @try {
                    for (PHAsset *asset in assetsToChange) {
                        PHAssetChangeRequest *change = [PHAssetChangeRequest changeRequestForAsset:asset];
                        if (change == nil) { changeException = @"PhotoKit did not create an asset change request"; return; }
                        change.creationDate = wanted[asset.localIdentifier];
                    }
                } @catch (NSException *exception) { changeException = exception.reason ?: @"PhotoKit rejected the creation-date change request"; }
            } completionHandler:^(BOOL success, NSError *error) {
                @autoreleasepool {
                    if (changeException != nil) { PKRequestFinish(request, PKFailure(result, @"change_rejected", changeException)); PKRequestRelease(request); return; }
                    if (!success) { PKRequestFinish(request, PKFailure(result, @"change_failed", error.localizedDescription ?: @"PhotoKit did not apply the creation-date change")); PKRequestRelease(request); return; }
                    dispatch_async(dispatch_get_global_queue(QOS_CLASS_USER_INITIATED, 0), ^{
                        @autoreleasepool {
                            NSDictionary *verification = nil;
                            for (NSUInteger attempt = 0; attempt < 10; attempt++) {
                                verification = PKVerifySetDates(photoIDs, wanted, changedIDs, alreadyIDs);
                                if ([verification[@"ok"] boolValue]) break;
                                if (attempt < 9) [NSThread sleepForTimeInterval:0.2];
                            }
                            PKRequestFinish(request, verification);
                            PKRequestRelease(request);
                        }
                    });
                }
            }];
            retainedForCompletion = NO;
        } @catch (NSException *exception) {
            PKRequestFinish(request, PKFailure(result, @"photokit_exception", exception.reason ?: @"PhotoKit rejected the request"));
            if (retainedForCompletion) PKRequestRelease(request);
        }
    }
    return request;
}
