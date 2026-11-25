// SPDX-License-Identifier: AGPL-3.0-only

package stripe

import (
	"errors"
	"fmt"
	"testing"

	stripego "github.com/stripe/stripe-go/v87"
)

func TestIsPrefixRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"prefix refused": {
			err:  fmt.Errorf("updating: %w", &stripego.Error{Type: stripego.ErrorTypeInvalidRequest, Param: "invoice_prefix", Msg: "already in use"}),
			want: true,
		},
		"another param": {
			err: &stripego.Error{Type: stripego.ErrorTypeInvalidRequest, Param: "email"},
		},
		"not a request error": {
			err: &stripego.Error{Type: stripego.ErrorTypeAPI, Param: "invoice_prefix"},
		},
		"not a Stripe error": {err: errors.New("timeout")},
		"no error":           {},
	} {
		t.Run(name, func(t *testing.T) {
			if got := isPrefixRefused(tc.err); got != tc.want {
				t.Fatalf("isPrefixRefused = %v, want %v", got, tc.want)
			}
		})
	}
}
