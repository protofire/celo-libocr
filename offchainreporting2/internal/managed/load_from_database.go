package managed

import (
	"context"

	"github.com/protofire/celo-libocr/commontypes"
	"github.com/protofire/celo-libocr/internal/loghelper"
	"github.com/protofire/celo-libocr/offchainreporting2/types"
)

func loadConfigFromDatabase(ctx context.Context, database types.Database, logger loghelper.LoggerWithContext) *types.ContractConfig {
	cc, err := database.ReadConfig(ctx)
	if err != nil {
		logger.ErrorIfNotCanceled("loadConfigFromDatabase: Error during Database.ReadConfig", ctx, commontypes.LogFields{
			"error": err,
		})
		return nil
	}

	if cc == nil {
		logger.Info("loadConfigFromDatabase: Database.ReadConfig returned nil, no configuration to restore", nil)
		return nil
	}

	return cc
}
