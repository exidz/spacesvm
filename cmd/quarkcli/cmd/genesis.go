// Copyright (C) 2019-2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package cmd

import (
	"os"
	"path/filepath"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/ava-labs/quarkvm/chain"
)

func init() {
	genesisCmd.PersistentFlags().StringVar(
		&genesisFile,
		"genesis-file",
		filepath.Join(workDir, "genesis.json"),
		"genesis file path",
	)
	genesisCmd.PersistentFlags().Int64Var(
		&minDifficulty,
		"min-difficulty",
		-1,
		"minimum difficulty for mining",
	)
	genesisCmd.PersistentFlags().Int64Var(
		&minBlockCost,
		"min-block-cost",
		-1,
		"minimum block cost",
	)
}

var (
	genesisFile   string
	minDifficulty int64
	minBlockCost  int64
)

var genesisCmd = &cobra.Command{
	Use:   "genesis [options]",
	Short: "Creates a new genesis in the default location",
	RunE:  genesisFunc,
}

func genesisFunc(cmd *cobra.Command, args []string) error {
	defaultGenesis := chain.DefaultGenesis()
	if minDifficulty >= 0 {
		defaultGenesis.MinDifficulty = uint64(minDifficulty)
	}
	if minBlockCost >= 0 {
		defaultGenesis.MinBlockCost = uint64(minBlockCost)
	}
	blk := &chain.StatefulBlock{
		Tmstmp:     time.Now().Unix(),
		Difficulty: defaultGenesis.MinDifficulty,
		Cost:       defaultGenesis.MinBlockCost,
		Genesis:    defaultGenesis,
	}
	b, err := chain.Marshal(blk)
	if err != nil {
		return err
	}
	if err := os.WriteFile(genesisFile, b, fsModeWrite); err != nil {
		return err
	}
	color.Green("created genesis and saved to %s", genesisFile)
	return nil
}
