package protocol

import (
	"github.com/protofire/celo-libocr/commontypes"
	"github.com/protofire/celo-libocr/offchainreporting2/types"
)

type TelemetrySender interface {
	RoundStarted(
		configDigest types.ConfigDigest,
		epoch uint32,
		round uint8,
		leader commontypes.OracleID,
	)
}
