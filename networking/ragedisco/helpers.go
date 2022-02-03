package ragedisco

import (
	"github.com/protofire/celo-libocr/commontypes"
	ragetypes "github.com/protofire/celo-libocr/ragep2p/types"
)

func equalAddrs(a []ragetypes.Address, b []ragetypes.Address) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func reason(err error) commontypes.LogFields {
	return commontypes.LogFields{"reason": err}
}
