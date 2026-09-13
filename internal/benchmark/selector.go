package benchmark

import (
	"cmp"
	"slices"
)

const SelectionUnbenchmarked = "unbenchmarked"
const SelectionMeasured = "measured"

func SelectBackend(request SelectionRequest, profiles []RuntimeProfile) Selection {
	candidates := make([]RuntimeProfile, 0, len(profiles))
	for _, profile := range profiles {
		if !profile.Eligible || !compatible(request, profile.Key) {
			continue
		}
		if request.SLO != nil && (profile.Measurements.TTFTP95MS > request.SLO.TTFTP95MS || profile.Measurements.TPOTP95MS > request.SLO.TPOTP95MS) {
			continue
		}
		candidates = append(candidates, profile)
	}
	if len(candidates) == 0 {
		return Selection{Backend: BackendVLLM, Status: SelectionUnbenchmarked}
	}
	slices.SortFunc(candidates, func(a, b RuntimeProfile) int {
		if order := cmp.Compare(a.Measurements.CostPerSuccessfulRequest, b.Measurements.CostPerSuccessfulRequest); order != 0 {
			return order
		}
		if order := cmp.Compare(a.Measurements.TTFTP95MS, b.Measurements.TTFTP95MS); order != 0 {
			return order
		}
		return cmp.Compare(string(a.Key.Backend), string(b.Key.Backend))
	})
	winner := candidates[0]
	measurements := winner.Measurements
	return Selection{Backend: winner.Key.Backend, ProfileID: winner.ID, Status: SelectionMeasured, Measurements: &measurements}
}

func compatible(request SelectionRequest, got ProfileKey) bool {
	want := request.KeyWithoutBackend
	runtimeMatches := want.BackendVersion == got.BackendVersion &&
		want.RuntimeImageDigest == got.RuntimeImageDigest
	if request.ExpectedRuntimes != nil {
		expected, ok := request.ExpectedRuntimes[got.Backend]
		runtimeMatches = ok && expected.BackendVersion == got.BackendVersion &&
			expected.RuntimeImageDigest == got.RuntimeImageDigest
	}
	return runtimeMatches && want.ModelURI == got.ModelURI &&
		want.ModelRevision == got.ModelRevision &&
		want.GPUSKU == got.GPUSKU &&
		want.GPUCount == got.GPUCount &&
		want.Precision == got.Precision &&
		want.Quantization == got.Quantization &&
		want.TensorParallelism == got.TensorParallelism &&
		want.MaxContextBucket == got.MaxContextBucket &&
		want.DriverCUDAFingerprint == got.DriverCUDAFingerprint &&
		want.ScenarioDigest == got.ScenarioDigest
}
