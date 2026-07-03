// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("setupIntentNeedsCancel", func() {
	It("skips succeeded and canceled intents", func() {
		Expect(setupIntentNeedsCancel(&stripev1alpha1.StripeSetupIntentStatus{
			ID:     "seti_1",
			Status: "succeeded",
		})).To(BeFalse())
		Expect(setupIntentNeedsCancel(&stripev1alpha1.StripeSetupIntentStatus{
			ID:     "seti_2",
			Status: "canceled",
		})).To(BeFalse())
	})

	It("cancels in-flight intents", func() {
		Expect(setupIntentNeedsCancel(&stripev1alpha1.StripeSetupIntentStatus{
			ID:     "seti_3",
			Status: "requires_payment_method",
		})).To(BeTrue())
		Expect(setupIntentNeedsCancel(&stripev1alpha1.StripeSetupIntentStatus{
			ID:     "seti_4",
			Status: "requires_confirmation",
		})).To(BeTrue())
	})

	It("ignores empty setup intent state", func() {
		Expect(setupIntentNeedsCancel(nil)).To(BeFalse())
		Expect(setupIntentNeedsCancel(&stripev1alpha1.StripeSetupIntentStatus{})).To(BeFalse())
	})
})
