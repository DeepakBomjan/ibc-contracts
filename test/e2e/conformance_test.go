package e2e_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	interchaintest "github.com/icon-project/ibc-integration/test"
	"github.com/icon-project/ibc-integration/test/chains"
	"github.com/icon-project/ibc-integration/test/relayer"
	"github.com/strangelove-ventures/interchaintest/v7/ibc"
	"github.com/strangelove-ventures/interchaintest/v7/testreporter"
	"github.com/stretchr/testify/require"

	"go.uber.org/zap/zaptest"
)

const (
	relayerImageEnv    = "RELAYER_IMAGE"
	relayerImage       = "relayer"
	relayerImageTagEnv = "RELAYER_IMAGE_TAG"
	relayerImageTag    = "latest"
)

func TestConformance(t *testing.T) {
	fmt.Println("test start")
	cfg, err := GetConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	logger := zaptest.NewLogger(t)

	chainFactory := NewBuiltinChainFactory(logger, cfg.ChainSpecs)

	_chains, err := chainFactory.Chains(t.Name())
	require.NoError(t, err)

	// TODO has to be gochain in current permissioned setup of btp blocks
	owner := "gochain"
	user := "user"

	chainA := _chains[0]
	chainB := _chains[1]
	client, network := interchaintest.DockerSetup(t)

	// Log location
	f, err := interchaintest.CreateLogFile(fmt.Sprintf("%d.json", time.Now().Unix()))
	if err != nil {
		return
	}
	optionDocker := relayer.CustomDockerImage(chains.GetEnvOrDefault(relayerImageEnv, relayerImage), chains.GetEnvOrDefault(relayerImageTagEnv, relayerImageTag), "100:1000")

	r := interchaintest.NewICONRelayerFactory(zaptest.NewLogger(t), optionDocker, relayer.ImagePull(false)).Build(t, client, network)
	// Reporter/logs
	rep := testreporter.NewReporter(f)
	eRep := rep.RelayerExecReporter(t)

	// Build interchain
	opts := ibc.CreateChannelOptions{
		SourcePortName: "mock",
		DestPortName:   "mock",
		Order:          ibc.Unordered,
		Version:        "ics20-1",
	}

	const ibcPath = "icon-cosmoshub"
	ic := interchaintest.NewInterchain().
		AddChain(chainA.(ibc.Chain)).
		AddChain(chainB.(ibc.Chain)).
		AddRelayer(r, "relayer").
		AddLink(interchaintest.InterchainLink{
			Chain1:            chainA.(ibc.Chain),
			Chain2:            chainB.(ibc.Chain),
			Relayer:           r,
			Path:              ibcPath,
			CreateChannelOpts: opts,
			CreateClientOpts: ibc.CreateClientOptions{
				TrustingPeriod: "100000m",
			},
		})

	require.NoError(t, ic.BuildChains(ctx, eRep, interchaintest.InterchainBuildOptions{
		TestName:          t.Name(),
		Client:            client,
		NetworkID:         network,
		BlockDatabaseFile: interchaintest.DefaultBlockDatabaseFilepath(),

		SkipPathCreation: false},
	))
	require.NoError(t, chainA.BuildWallets(ctx, owner))
	require.NoError(t, chainB.BuildWallets(ctx, owner))

	require.NoError(t, chainA.BuildWallets(ctx, user))
	require.NoError(t, chainB.BuildWallets(ctx, user))

	ctx, err = chainA.SetupIBC(ctx, owner)
	require.NoError(t, err)
	contracts1 := ctx.Value(chains.Mykey("Contract Names")).(chains.ContractKey)
	require.NoError(t, err)
	ctx, err = chainB.SetupIBC(ctx, owner)
	require.NoError(t, err)

	contracts2 := ctx.Value(chains.Mykey("Contract Names")).(chains.ContractKey)
	fmt.Println(contracts1.ContractAddress)
	fmt.Println(contracts2.ContractAddress)
	if chainA.(ibc.Chain).Config().Type == "icon" {
		chainA.OverrideConfig("archway-handler-address", contracts2.ContractAddress["ibc"])
	}

	if chainB.(ibc.Chain).Config().Type == "icon" {
		chainB.OverrideConfig("archway-handler-address", contracts1.ContractAddress["ibc"])
	}

	// Start the Relay
	require.NoError(t, ic.BuildRelayer(ctx, eRep, interchaintest.InterchainBuildOptions{
		TestName:          t.Name(),
		Client:            client,
		NetworkID:         network,
		BlockDatabaseFile: interchaintest.DefaultBlockDatabaseFilepath(),

		SkipPathCreation: false},
	))

	require.NoError(t, r.StartRelayer(ctx, eRep, ibcPath))
	nid1 := cfg.ChainSpecs[0].ChainConfig.ChainID
	nid2 := cfg.ChainSpecs[1].ChainConfig.ChainID

	// TODO get channel from relay
	chainA.ConfigureBaseConnection(context.Background(), owner, "channel-0", nid2, contracts2.ContractAddress["connection"])
	chainB.ConfigureBaseConnection(context.Background(), owner, "channel-0", nid1, contracts1.ContractAddress["connection"])

	msg := "Hello"
	dst := nid2 + "/" + contracts2.ContractAddress["dapp"]
	_, reqId, err := chainA.XCall(context.Background(), chainB, user, dst, []byte(msg), nil)
	ctx, err = chainB.ExecuteCall(ctx, reqId)
	require.NoError(t, err)
	fmt.Println(ctx.Value("txResult"))

	msg = "rollback"
	rollback := "rollback data"
	sn, reqId, err := chainA.XCall(context.Background(), chainB, user, dst, []byte(msg), []byte(rollback))
	require.NoError(t, err)

	ctx, err = chainB.ExecuteCall(ctx, reqId)
	require.NoError(t, err)
	fmt.Println(ctx.Value("txResult"))
	time.Sleep(10 * time.Second)

	ctx, err = chainA.ExecuteRollback(ctx, sn)
	require.NoError(t, err)
	fmt.Println(ctx.Value("txResult"))
	r.StopRelayer(ctx, eRep)
}
