// SPDX-License-Identifier: AGPL-3.0-only

package stripe

import (
	"context"
	"errors"
	"fmt"

	stripego "github.com/stripe/stripe-go/v87"
)

// ErrInvoicePrefixTaken is returned by SetCustomerInvoicePrefix when Stripe
// refuses the prefix, because another customer has or once had it. Stripe
// never lets a prefix be used twice, even after its customer is deleted.
var ErrInvoicePrefixTaken = errors.New("the invoice prefix is already taken in Stripe")

// CustomerInvoicePrefix reads the prefix Stripe numbers a customer's
// invoices with.
func (c *Client) CustomerInvoicePrefix(ctx context.Context, customerID string) (string, error) {
	cus, err := c.api.V1Customers.Retrieve(ctx, customerID, &stripego.CustomerRetrieveParams{})
	if err != nil {
		return "", fmt.Errorf("retrieving Stripe customer %s: %w", customerID, err)
	}
	return cus.InvoicePrefix, nil
}

// SetCustomerInvoicePrefix sets the prefix Stripe numbers a customer's
// invoices with from now on. Invoices already finalized keep their numbers.
func (c *Client) SetCustomerInvoicePrefix(ctx context.Context, customerID, prefix string) error {
	_, err := c.api.V1Customers.Update(ctx, customerID, &stripego.CustomerUpdateParams{InvoicePrefix: stripego.String(prefix)})
	if isPrefixRefused(err) {
		return fmt.Errorf("%w: %s: %w", ErrInvoicePrefixTaken, prefix, err)
	}
	if err != nil {
		return fmt.Errorf("setting invoice prefix %s on Stripe customer %s: %w", prefix, customerID, err)
	}
	return nil
}

// isPrefixRefused reports whether Stripe refused the request because of
// its invoice_prefix. Stripe has no error code for a taken prefix, and the
// provider only sends prefixes of 1 to 12 digits, which are well formed,
// so a refusal of the prefix means it's taken.
func isPrefixRefused(err error) bool {
	var se *stripego.Error
	return errors.As(err, &se) && se.Type == stripego.ErrorTypeInvalidRequest && se.Param == "invoice_prefix"
}
