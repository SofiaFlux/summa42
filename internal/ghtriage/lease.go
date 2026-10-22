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

// validationKillGrace is the coreutils --kill-after grace on a validation command.
const validationKillGrace = 5 * time.Second

// archiveStreamingCap bounds streaming the candidate archive into the validation
// user.
const archiveStreamingCap = 5 * time.Minute

// ImplementationLease is the implementation attempt lease for a model timeout
// (0 = default). It must outlive the model call plus all of the validation
// commands (each validation timeout plus its kill grace), the archive streaming
// cap, and the stage margin.
func ImplementationLease(modelTimeout time.Duration, validationCommands int, validationTimeout time.Duration) time.Duration {
	if modelTimeout <= 0 && validationCommands <= 0 {
		return ImplementationLeaseDuration
	}
	if validationTimeout <= 0 {
		validationTimeout = 10 * time.Minute
	}
	lease := max(modelTimeout, 0) + time.Duration(max(validationCommands, 0))*(validationTimeout+validationKillGrace) + archiveStreamingCap + implementationLeaseMargin
	return max(lease, ImplementationLeaseDuration)
}
