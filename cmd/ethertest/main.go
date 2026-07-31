package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/BurntSushi/toml"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/islishude/ethertest"
	"github.com/urfave/cli/v2"
)

func main() {
	app := &cli.App{
		Name: "ethertest", Usage: "local execution and consensus test node",
		Version: ethertest.Version, EnableBashCompletion: true,
		Flags:  commonFlags(),
		Action: runNode,
		Commands: []*cli.Command{
			configCommand(), networkCommand(), blobCommand(), stateCommand(),
			accountsCommand(), capabilitiesCommand(), completionCommand(),
		},
	}
	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "ethertest:", err)
		os.Exit(1)
	}
}

func commonFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "config", Usage: "strict TOML configuration file"},
		&cli.Uint64Flag{Name: "chain-id"},
		&cli.Int64Flag{Name: "genesis-time"},
		&cli.StringFlag{Name: "http", Usage: "shared HTTP+WS listen address"},
		&cli.BoolFlag{Name: "no-http"},
		&cli.BoolFlag{Name: "no-beacon"},
		&cli.BoolFlag{Name: "allow-unsafe-external"},
		&cli.StringFlag{Name: "data-dir", Usage: "enable Pebble at this directory"},
		&cli.StringFlag{Name: "dump-state", Usage: "atomically dump state on shutdown"},
		&cli.StringFlag{Name: "log-level", Usage: "debug, info, warn, error, or off"},
		&cli.BoolFlag{Name: "log-json", Usage: "write newline-delimited JSON logs"},
		&cli.DurationFlag{Name: "log-progress-interval", Usage: "aggregate automatic mining progress over this interval"},
	}
}

func effectiveConfig(ctx *cli.Context) (ethertest.Config, error) {
	cfg, err := ethertest.ReadConfig(ctx.String("config"))
	if err != nil {
		return ethertest.Config{}, err
	}
	if ctx.IsSet("chain-id") {
		cfg.Chain.ChainID, cfg.Chain.NetworkID = ctx.Uint64("chain-id"), ctx.Uint64("chain-id")
	}
	if ctx.IsSet("genesis-time") {
		cfg.Chain.GenesisTime = ctx.Int64("genesis-time")
	}
	if ctx.IsSet("http") {
		cfg.HTTP.Address = ctx.String("http")
	}
	if ctx.Bool("no-http") {
		cfg.HTTP.Enabled, cfg.Beacon.Enabled = false, false
	}
	if ctx.Bool("no-beacon") {
		cfg.Beacon.Enabled = false
	}
	if ctx.Bool("allow-unsafe-external") {
		cfg.HTTP.AllowUnsafeExternal = true
	}
	if ctx.IsSet("data-dir") {
		cfg.Storage.Engine, cfg.Storage.Path = "pebble", ctx.String("data-dir")
	}
	if ctx.IsSet("dump-state") {
		cfg.DumpState = ctx.String("dump-state")
	}
	if ctx.IsSet("log-level") {
		cfg.Log.Level = ctx.String("log-level")
	}
	if ctx.IsSet("log-json") {
		cfg.Log.JSON = ctx.Bool("log-json")
	}
	if ctx.IsSet("log-progress-interval") {
		cfg.Log.ProgressInterval = ctx.Duration("log-progress-interval")
	}
	return cfg, cfg.Validate()
}

func runNode(ctx *cli.Context) error {
	cfg, err := effectiveConfig(ctx)
	if err != nil {
		return err
	}
	logger := newLogger(cfg.Log, os.Stdout)
	node, err := ethertest.New(cfg, ethertest.WithLogger(logger))
	if err != nil {
		return err
	}
	if err := node.Start(); err != nil {
		return err
	}
	printDevelopmentAccounts(os.Stderr, cfg)
	signalContext, stop := signal.NotifyContext(ctx.Context, os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-signalContext.Done()
	return node.Close()
}

// printDevelopmentAccounts is intentionally separate from structured runtime
// logging: the human-only output contains public development private keys.
func printDevelopmentAccounts(output io.Writer, cfg ethertest.Config) {
	if cfg.Log.JSON {
		return
	}
	accounts, _ := ethertest.DeriveAccounts(cfg.Accounts.Mnemonic, cfg.Accounts.Count)
	fmt.Fprintln(output, "Unlocked development accounts (never use these keys on a real network):")
	for index, account := range accounts {
		fmt.Fprintf(output, "  (%d) %s  %s  %s\n", index, account.Address, hex.EncodeToString(crypto.FromECDSA(account.PrivateKey)), account.Path)
	}
}

func newLogger(cfg ethertest.LogConfig, output io.Writer) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	case "off":
		level = slog.Level(100)
	}
	options := &slog.HandlerOptions{Level: level}
	if cfg.JSON {
		return slog.New(slog.NewJSONHandler(output, options))
	}
	return slog.New(slog.NewTextHandler(output, options))
}

func configCommand() *cli.Command {
	return &cli.Command{
		Name: "config", Subcommands: []*cli.Command{
			{Name: "print", Action: func(ctx *cli.Context) error {
				cfg, err := effectiveConfig(ctx)
				if err != nil {
					return err
				}
				return toml.NewEncoder(os.Stdout).Encode(cfg)
			}},
			{Name: "validate", Action: func(ctx *cli.Context) error {
				_, err := effectiveConfig(ctx)
				if err == nil {
					fmt.Println("configuration is valid")
				}
				return err
			}},
		},
	}
}

func networkCommand() *cli.Command {
	return &cli.Command{Name: "network", Flags: []cli.Flag{&cli.BoolFlag{Name: "json"}}, Action: func(ctx *cli.Context) error {
		cfg, err := effectiveConfig(ctx)
		if err != nil {
			return err
		}
		executionEndpoint, beaconEndpoint := configuredEndpoints(cfg)
		value := map[string]any{
			"chainId": cfg.Chain.ChainID, "networkId": cfg.Chain.NetworkID,
			"genesisTime": cfg.Chain.GenesisTime, "fork": "osaka/fulu",
			"execution": executionEndpoint, "consensus": beaconEndpoint,
			"syntheticFinality": true, "consensusMode": "synthetic",
			"beaconApi": "v4-subset", "fullConsensus": false, "releaseComplete": false,
		}
		return json.NewEncoder(os.Stdout).Encode(value)
	}}
}

func configuredEndpoints(cfg ethertest.Config) (string, string) {
	if !cfg.HTTP.Enabled {
		return "", ""
	}
	if !cfg.Beacon.Enabled {
		return cfg.HTTP.Address, ""
	}
	return cfg.HTTP.Address, cfg.HTTP.Address
}

func blobCommand() *cli.Command {
	return &cli.Command{Name: "blob", Subcommands: []*cli.Command{
		{Name: "encode", Usage: "encode payload-file blob-file", Flags: []cli.Flag{
			&cli.StringFlag{Name: "codec", Value: "packed-bytes-v1"},
		}, Action: blobEncode},
		{Name: "decode", Usage: "decode blob-file payload-file", Action: blobDecode},
		{Name: "send", Usage: "send payload-file", Flags: []cli.Flag{
			&cli.StringFlag{Name: "rpc", Value: "http://127.0.0.1:8545"},
			&cli.IntFlag{Name: "account", Value: 0},
			&cli.StringFlag{Name: "to", Value: "0x0000000000000000000000000000000000000000"},
		}, Action: blobSend},
	}}
}

func blobEncode(ctx *cli.Context) error {
	if ctx.NArg() != 2 {
		return cli.Exit("usage: ethertest blob encode PAYLOAD BLOB", 2)
	}
	input, err := os.ReadFile(ctx.Args().Get(0))
	if err != nil {
		return err
	}
	var blob kzg4844.Blob
	switch ctx.String("codec") {
	case "packed-bytes-v1":
		blob, err = ethertest.EncodePackedBytesV1(input)
	case "canonical-blob":
		if len(input) != len(blob) {
			return fmt.Errorf("canonical blob must be exactly %d bytes", len(blob))
		}
		copy(blob[:], input)
	default:
		return errors.New("unsupported blob codec")
	}
	if err != nil {
		return err
	}
	return os.WriteFile(ctx.Args().Get(1), blob[:], 0o600)
}

func blobDecode(ctx *cli.Context) error {
	if ctx.NArg() != 2 {
		return cli.Exit("usage: ethertest blob decode BLOB PAYLOAD", 2)
	}
	input, err := os.ReadFile(ctx.Args().Get(0))
	if err != nil {
		return err
	}
	if len(input) != 131072 {
		return errors.New("invalid canonical blob length")
	}
	var blob kzg4844.Blob
	copy(blob[:], input)
	payload, err := ethertest.DecodePackedBytesV1(blob)
	if err != nil {
		return err
	}
	return os.WriteFile(ctx.Args().Get(1), payload, 0o600)
}

func blobSend(ctx *cli.Context) error {
	if ctx.NArg() != 1 {
		return cli.Exit("usage: ethertest blob send PAYLOAD", 2)
	}
	payload, err := os.ReadFile(ctx.Args().First())
	if err != nil {
		return err
	}
	blob, err := ethertest.EncodePackedBytesV1(payload)
	if err != nil {
		return err
	}
	accounts, err := ethertest.DeriveAccounts(ethertest.DefaultMnemonic, 10)
	if err != nil || ctx.Int("account") < 0 || ctx.Int("account") >= len(accounts) {
		return errors.New("invalid account")
	}
	client, err := rpc.DialContext(ctx.Context, ctx.String("rpc"))
	if err != nil {
		return err
	}
	defer client.Close()
	account := accounts[ctx.Int("account")]
	var nonce hexutil.Uint64
	if err := client.CallContext(ctx.Context, &nonce, "eth_getTransactionCount", account.Address, "pending"); err != nil {
		return err
	}
	var chainID hexutil.Uint64
	if err := client.CallContext(ctx.Context, &chainID, "eth_chainId"); err != nil {
		return err
	}
	tx, err := ethertest.SignBlobTransaction(ethertest.BlobTransactionRequest{
		ChainID: new(big.Int).SetUint64(uint64(chainID)), Nonce: uint64(nonce),
		To: common.HexToAddress(ctx.String("to")), Gas: 100_000,
		GasTipCap: big.NewInt(1_000_000_000), GasFeeCap: big.NewInt(3_000_000_000),
		BlobFeeCap: big.NewInt(1_000_000_000), Blob: blob,
	}, account.PrivateKey)
	if err != nil {
		return err
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		return err
	}
	var hash common.Hash
	if err := client.CallContext(ctx.Context, &hash, "eth_sendRawTransaction", hexutil.Bytes(raw)); err != nil {
		return err
	}
	fmt.Println(hash)
	return nil
}

func stateCommand() *cli.Command {
	return &cli.Command{Name: "state", Subcommands: []*cli.Command{
		{Name: "inspect", Action: func(ctx *cli.Context) error {
			if ctx.NArg() != 1 {
				return cli.Exit("usage: ethertest state inspect ARCHIVE", 2)
			}
			manifest, err := ethertest.InspectState(ctx.Args().First())
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(manifest)
		}},
		{Name: "dump", Action: func(ctx *cli.Context) error {
			if ctx.NArg() != 1 {
				return cli.Exit("usage: ethertest state dump ARCHIVE --data-dir DIR", 2)
			}
			cfg, err := effectiveConfig(ctx)
			if err != nil {
				return err
			}
			cfg.HTTP.Enabled, cfg.Beacon.Enabled = false, false
			node, err := ethertest.New(cfg)
			if err != nil {
				return err
			}
			defer node.Close() //nolint:errcheck
			return node.DumpState(ctx.Args().First())
		}},
		{Name: "load", Flags: []cli.Flag{&cli.StringFlag{Name: "to", Required: true}}, Action: func(ctx *cli.Context) error {
			if ctx.NArg() != 1 {
				return cli.Exit("usage: ethertest state load ARCHIVE --to DIR", 2)
			}
			return ethertest.LoadState(ctx.Args().First(), ctx.String("to"))
		}},
	}}
}

func accountsCommand() *cli.Command {
	return &cli.Command{Name: "accounts", Subcommands: []*cli.Command{
		{Name: "export", Flags: []cli.Flag{&cli.BoolFlag{Name: "unsafe-plain"}}, Action: func(ctx *cli.Context) error {
			if !ctx.Bool("unsafe-plain") {
				return errors.New("refusing plaintext key export without --unsafe-plain")
			}
			cfg, err := effectiveConfig(ctx)
			if err != nil {
				return err
			}
			accounts, err := ethertest.DeriveAccounts(cfg.Accounts.Mnemonic, cfg.Accounts.Count)
			if err != nil {
				return err
			}
			for _, account := range accounts {
				fmt.Printf("%s %s %s\n", account.Address, hex.EncodeToString(crypto.FromECDSA(account.PrivateKey)), account.Path)
			}
			return nil
		}},
	}}
}

func capabilitiesCommand() *cli.Command {
	return &cli.Command{Name: "capabilities", Action: func(*cli.Context) error {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"version": ethertest.Version, "status": "alpha", "fork": "osaka/fulu",
			"syntheticFinality": true, "blobCodec": []string{"canonical-blob", "packed-bytes-v1"},
			"consensusMode": "synthetic", "beaconApi": "v4-subset", "fullConsensus": false,
			"forkTransitions": []string{"deneb", "electra", "fulu"},
			"releaseComplete": false,
		})
	}}
}

func completionCommand() *cli.Command {
	return &cli.Command{Name: "completion", Action: func(ctx *cli.Context) error {
		shell := ctx.Args().First()
		switch shell {
		case "bash":
			fmt.Println(`complete -o bashdefault -o default -o nospace -C ethertest ethertest`)
		case "zsh":
			fmt.Println(`#compdef ethertest
_ethertest() { local -a commands; commands=(config network blob state accounts capabilities completion); _describe commands commands; }`)
		case "fish":
			fmt.Println(`complete -c ethertest -f -a "config network blob state accounts capabilities completion"`)
		default:
			return errors.New("completion shell must be bash, zsh, or fish")
		}
		return nil
	}}
}
