package ghtriage

import "time"

// A lease that wraps a model call must outlive that call. With no configured
// model timeout the historical fixed leases apply; with one, the lease is the
// model timeout plus the stage's non-model margin, so a slow but healthy model
// call is never cut off by its own lease.
const (
	reviewLeaseMargin         = 2 * time.Minute
	implementationLeaseMargin = 15 * time.Minute
)

func leaseFor(modelTimeout, margin, fixed time.Duration) time.Duration {
	if modelTimeout <= 0 {
		return fixed
	}
	return modelTimeout + margin
}

// PlanReviewLease is the plan-review attempt lease for a model timeout (0 = default).
func PlanReviewLease(modelTimeout time.Duration) time.Duration {
	return leaseFor(modelTimeout, reviewLeaseMargin, PlanReviewLeaseDuration)
}

// CodeReviewLease is the code-review attempt lease for a model timeout (0 = default).
func CodeReviewLease(modelTimeout time.Duration) time.Duration {
	return leaseFor(modelTimeout, reviewLeaseMargin, CodeReviewLeaseDuration)
}

// ImplementationLease is the implementation attempt lease for a model timeout
// (0 = default). The margin covers checkout and the validation commands.
func ImplementationLease(modelTimeout time.Duration) time.Duration {
	return leaseFor(modelTimeout, implementationLeaseMargin, ImplementationLeaseDuration)
}
