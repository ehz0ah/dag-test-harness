package openviking

import "code.byted.org/data-arch/ovtest/ops"

var (
	DeleteAccount      = ops.OvDeleteAccount
	CreateAccount      = ops.OvCreateAccount
	Command            = ops.OvCommand
	AddMemory          = ops.OvAddMemory
	Wait               = ops.OvWait
	List               = ops.OvList
	Find               = ops.OvFind
	Search             = ops.OvSearch
	URIAbsent          = ops.OvURIAbsent
	Remove             = ops.OvRemove
	SessionNew         = ops.OvSessionNew
	SessionAddMessage  = ops.OvSessionAddMessage
	SessionAddMessages = ops.OvSessionAddMessages
	SessionCommit      = ops.OvSessionCommit
	SessionPresent     = ops.OvSessionPresent
	SessionCommitted   = ops.OvSessionCommitted
	Judge              = ops.OvJudge
)
