package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/osac-project/osac/osac-operator/api/v1alpha1"
)

var _ = Describe("VirtualNetwork transit teardown condition", func() {
	It("keeps a failed transit teardown visible until a later cleanup succeeds", func() {
		base := time.Now().UTC()
		failed := v1alpha1.JobStatus{
			Type:      v1alpha1.JobTypeDeprovision,
			State:     v1alpha1.JobStateFailed,
			Message:   "TRANSIT_TEARDOWN_STUCK: fabric rejected deletion",
			Timestamp: metav1.NewTime(base),
		}
		Expect(transitTeardownStuckMessage([]v1alpha1.JobStatus{failed})).To(ContainSubstring("TRANSIT_TEARDOWN_STUCK"))

		succeeded := v1alpha1.JobStatus{
			Type:      v1alpha1.JobTypeDeprovision,
			State:     v1alpha1.JobStateSucceeded,
			Timestamp: metav1.NewTime(base.Add(time.Second)),
		}
		Expect(transitTeardownStuckMessage([]v1alpha1.JobStatus{failed, succeeded})).To(BeEmpty())
	})
})
