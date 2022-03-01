// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package task

import (
	"context"

	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	berrors "github.com/pingcap/br/pkg/errors"
	"github.com/pingcap/br/pkg/glue"
	"github.com/pingcap/br/pkg/restore"
	"github.com/pingcap/br/pkg/summary"
	"github.com/pingcap/br/pkg/utils"
)

// RestoreRawConfig is the configuration specific for raw kv restore tasks.
type RestoreRawConfig struct {
	RawKvConfig
	RestoreCommonConfig
}

// DefineRawRestoreFlags defines common flags for the backup command.
func DefineRawRestoreFlags(command *cobra.Command) {
	command.Flags().StringP(flagKeyFormat, "", "hex", "start/end key format, support raw|escaped|hex")
	command.Flags().StringP(flagTiKVColumnFamily, "", "default", "restore specify cf, correspond to tikv cf")
	command.Flags().StringP(flagStartKey, "", "", "restore raw kv start key, key is inclusive")
	command.Flags().StringP(flagEndKey, "", "", "restore raw kv end key, key is exclusive")

	DefineRestoreCommonFlags(command.PersistentFlags())
}

// ParseFromFlags parses the backup-related flags from the flag set.
func (cfg *RestoreRawConfig) ParseFromFlags(flags *pflag.FlagSet) error {
	var err error
	cfg.Online, err = flags.GetBool(flagOnline)
	if err != nil {
		return errors.Trace(err)
	}
	err = cfg.RestoreCommonConfig.ParseFromFlags(flags)
	if err != nil {
		return errors.Trace(err)
	}
	return cfg.RawKvConfig.ParseFromFlags(flags)
}

func (cfg *RestoreRawConfig) adjust() {
	cfg.Config.adjust()
	cfg.RestoreCommonConfig.adjust()

	if cfg.Concurrency == 0 {
		cfg.Concurrency = defaultRestoreConcurrency
	}
}

// RunRestoreRaw starts a raw kv restore task inside the current goroutine.
func RunRestoreRaw(c context.Context, g glue.Glue, cmdName string, cfg *RestoreRawConfig) (err error) {
	cfg.adjust()

	defer summary.Summary(cmdName)
	ctx, cancel := context.WithCancel(c)
	defer cancel()

	// Restore raw does not need domain.
	needDomain := false
	mgr, err := NewMgr(ctx, g, cfg.PD, cfg.TLS, GetKeepalive(&cfg.Config), cfg.CheckRequirements, needDomain)
	if err != nil {
		return errors.Trace(err)
	}
	defer mgr.Close()

	keepaliveCfg := GetKeepalive(&cfg.Config)
	// sometimes we have pooled the connections.
	// sending heartbeats in idle times is useful.
	keepaliveCfg.PermitWithoutStream = true
	client, err := restore.NewRestoreClient(g, mgr.GetPDClient(), mgr.GetStorage(), mgr.GetTLSConfig(), keepaliveCfg)
	if err != nil {
		return errors.Trace(err)
	}
	defer client.Close()
	client.SetRateLimit(cfg.RateLimit)
	client.SetConcurrency(uint(cfg.Concurrency))
	if cfg.Online {
		client.EnableOnline()
	}
	client.SetSwitchModeInterval(cfg.SwitchModeInterval)

	u, _, backupMeta, err := ReadBackupMeta(ctx, utils.MetaFile, &cfg.Config)
	if err != nil {
		return errors.Trace(err)
	}
	g.Record(summary.RestoreDataSize, utils.ArchiveSize(backupMeta))
	if err = client.InitBackupMeta(c, backupMeta, u); err != nil {
		return errors.Trace(err)
	}

	if !client.IsRawKvMode() {
		return errors.Annotate(berrors.ErrRestoreModeMismatch, "cannot do raw restore from transactional data")
	}

	files, err := client.GetFilesInRawRange(cfg.StartKey, cfg.EndKey, cfg.CF)
	if err != nil {
		return errors.Trace(err)
	}

	if len(files) == 0 {
		log.Info("all files are filtered out from the backup archive, nothing to restore")
		return nil
	}
	summary.CollectInt("restore files", len(files))

	ranges, _, err := restore.MergeFileRanges(
		files, cfg.MergeSmallRegionKeyCount, cfg.MergeSmallRegionKeyCount)
	if err != nil {
		return errors.Trace(err)
	}

	// Redirect to log if there is no log file to avoid unreadable output.
	// TODO: How to show progress?
	updateCh := g.StartProgress(
		ctx,
		"Raw Restore",
		// Split/Scatter + Download/Ingest
		int64(len(ranges)+len(files)),
		!cfg.LogProgress)

	// RawKV restore does not need to rewrite keys.
	// rewrite := &restore.RewriteRules{}

	// NOTE: TiKV 5.0 do not support split ranges for RawKV keys.
	// err = restore.SplitRanges(ctx, client, ranges, rewrite, updateCh)
	// if err != nil {
	// 	return errors.Trace(err)
	// }

	restoreSchedulers, err := restorePreWork(ctx, client, mgr)
	if err != nil {
		return errors.Trace(err)
	}
	defer restorePostWork(ctx, client, restoreSchedulers)

	err = client.RestoreRaw(ctx, cfg.StartKey, cfg.EndKey, files, updateCh)
	if err != nil {
		return errors.Trace(err)
	}

	// Restore has finished.
	updateCh.Close()

	// Set task summary to success status.
	summary.SetSuccessStatus(true)
	return nil
}
