package openclaw

import "code.byted.org/data-arch/ovtest/runner"

func All() []runner.Case {
	return []runner.Case{
		memoryCase(),
		toolsCase(),
		compactionCase(),
	}
}
