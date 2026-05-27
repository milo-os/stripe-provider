// SPDX-License-Identifier: AGPL-3.0-only

package webhook

import "net/http"

// OkResponse signals successful processing. Stripe treats any 2xx as a
// delivered event.
func OkResponse() Response { return Response{HttpStatus: http.StatusOK} }

// BadRequestResponse signals a malformed or unprocessable payload.
// Stripe will not retry on 4xx (except 408 / 429) so reserve this for
// genuinely irrecoverable inputs.
func BadRequestResponse() Response { return Response{HttpStatus: http.StatusBadRequest} }

// UnauthorizedResponse signals a signature verification failure.
func UnauthorizedResponse() Response { return Response{HttpStatus: http.StatusUnauthorized} }

// MethodNotAllowedResponse signals a non-POST request.
func MethodNotAllowedResponse() Response { return Response{HttpStatus: http.StatusMethodNotAllowed} }

// InternalServerErrorResponse signals a transient failure. Stripe will
// retry with backoff (~3 days).
func InternalServerErrorResponse() Response { return Response{HttpStatus: http.StatusInternalServerError} }
