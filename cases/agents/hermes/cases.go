package hermes

import "code.byted.org/data-arch/ovtest/runner"

func All() []runner.Case {
	return []runner.Case{
		syncTurnCase(),
		toolsCase(),
	}
}
