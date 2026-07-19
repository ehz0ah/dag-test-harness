package openviking

import (
	"crypto/rand"
	"encoding/hex"

	"code.byted.org/data-arch/ovtest/runner"
)

func All() []runner.Case {
	return []runner.Case{
		serviceBaselineCase(),
		ovMemoryCase(),
		ovMemoryDirectCase(),
		ovMemoryUpdateCase(),
		ovExperienceLearningCase(),
		ovNegativeRecallCase(),
		ovRetrievalPrecisionCase(),
		ovForgetGhostCase(),
		ovSearchCompareCase(),
		ovMemoryCJKCase(),
	}
}

func nonce(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
