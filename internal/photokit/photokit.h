#ifndef PHOTOSCLI_PHOTOKIT_H
#define PHOTOSCLI_PHOTOKIT_H

#include <stddef.h>

typedef struct PKRequest PKRequest;

char *PKCopyAuthorizationStatusJSON(void);
char *PKCopyRemoveResultEncodingProbeJSON(int execute);
char *PKCopySetHiddenResultEncodingProbeJSON(int execute);
PKRequest *PKStartAuthorizationRequest(void);
PKRequest *PKStartRemoveFromAlbum(const char *albumID,
                                  const char *const *photoIDs,
                                  size_t photoIDCount,
                                  int execute);
PKRequest *PKStartSetHidden(const char *const *photoIDs,
                            size_t photoIDCount,
                            int hidden,
                            int execute);
PKRequest *PKStartSetCreationDates(const char *const *photoIDs,
                                   const double *epochSeconds,
                                   size_t photoIDCount,
                                   int execute);

// Returns 1 when complete, 0 on timeout, and -1 for an invalid request.
int PKRequestWait(PKRequest *request, int timeoutMilliseconds);
char *PKRequestCopyResultJSON(PKRequest *request);
void PKRequestRelease(PKRequest *request);
void PKFreeString(char *string);

#endif
