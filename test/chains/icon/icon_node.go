package icon

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	interchaintest "github.com/icon-project/ibc-integration/test"
	"github.com/icza/dyno"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/icon-project/ibc-integration/test/chains"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	dockerclient "github.com/docker/docker/client"
	"github.com/icon-project/ibc-integration/test/internal/blockdb"
	"github.com/icon-project/ibc-integration/test/internal/dockerutil"
	iconclient "github.com/icon-project/icon-bridge/cmd/iconbridge/chain/icon"
	icontypes "github.com/icon-project/icon-bridge/cmd/iconbridge/chain/icon/types"
	iconlog "github.com/icon-project/icon-bridge/common/log"
	"github.com/strangelove-ventures/interchaintest/v7/ibc"
	"go.uber.org/zap"
)

const (
	rpcPort              = "9080/tcp"
	GOLOOP_IMAGE_ENV     = "GOLOOP_IMAGE"
	GOLOOP_IMAGE         = "iconloop/goloop-icon"
	GOLOOP_IMAGE_TAG_ENV = "GOLOOP_IMAGE_TAG"
	GOLOOP_IMAGE_TAG     = "latest"
)

type IconNode struct {
	VolumeName   string
	Index        int
	Chain        ibc.Chain
	NetworkID    string
	DockerClient *dockerclient.Client
	Client       iconclient.Client
	TestName     string
	Image        ibc.DockerImage
	log          *zap.Logger
	ContainerID  string
	// Ports set during StartContainer.
	HostRPCPort string
	Validator   bool
	lock        sync.Mutex
	Address     string
}

type IconNodes []*IconNode

// Name of the test node container
func (in *IconNode) Name() string {
	var nodeType string
	if in.Validator {
		nodeType = "val"
	} else {
		nodeType = "fn"
	}
	return fmt.Sprintf("%s-%s-%d-%s", in.Chain.Config().ChainID, nodeType, in.Index, dockerutil.SanitizeContainerName(in.TestName))
}

// Create Node Container with ports exposed and published for host to communicate with
func (in *IconNode) CreateNodeContainer(ctx context.Context, additionalGenesisWallets ...ibc.WalletAmount) error {
	imageRef := in.Image.Ref()
	testBasePath := os.Getenv(chains.BASE_PATH)
	binds := in.Bind()
	binds = append(binds, fmt.Sprintf("%s/test/chains/icon/data/governance:%s", testBasePath, "/goloop/data/gov"))
	containerConfig := &types.ContainerCreateConfig{
		Config: &container.Config{
			Image:    imageRef,
			Hostname: in.HostName(),
			Labels:   map[string]string{dockerutil.CleanupLabel: in.TestName},
		},

		HostConfig: &container.HostConfig{
			Binds:           binds,
			PublishAllPorts: true,
			AutoRemove:      false,
			DNS:             []string{},
		},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				in.NetworkID: {},
			},
		},
	}
	cc, err := in.DockerClient.ContainerCreate(ctx, containerConfig.Config, containerConfig.HostConfig, containerConfig.NetworkingConfig, nil, in.Name())
	if err != nil {
		in.log.Error("Failed to create container", zap.Error(err))
		return err
	}

	err = in.modifyGenesisToAddGenesisAccount(ctx, cc.ID, additionalGenesisWallets...)
	if err != nil {
		in.log.Error("Failed to update genesis file in container", zap.Error(err))
		return err
	}

	in.ContainerID = cc.ID
	return nil
}
func (in *IconNode) CopyFileToContainer(ctx context.Context, content []byte, containerID, target string) error {
	header := ctx.Value("file-header").(map[string]string)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err := tw.WriteHeader(&tar.Header{
		Name: header["name"],
		Mode: 0644,
		Size: int64(len(content)),
	})
	_, err = tw.Write(content)
	if err != nil {
		return err
	}
	err = tw.Close()
	if err != nil {
		return err
	}
	if err := in.DockerClient.CopyToContainer(context.Background(), containerID, target, &buf, types.CopyToContainerOptions{}); err != nil {
		return fmt.Errorf("failed to upload file: %w", err)
	}
	return nil
}

func (in *IconNode) modifyGenesisToAddGenesisAccount(ctx context.Context, containerID string, additionalGenesisWallets ...ibc.WalletAmount) error {
	g := make(map[string]interface{})
	fileName := fmt.Sprintf("%s/test/chains/icon/data/genesis.json", os.Getenv(chains.BASE_PATH))

	genbz, err := interchaintest.GetLocalFileContent(fileName)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(genbz, &g); err != nil {
		return fmt.Errorf("failed to unmarshal genesis file: %w", err)
	}

	for index, wallet := range additionalGenesisWallets {
		genesisAccount := map[string]string{
			"address": wallet.Address,
			"balance": "0xd3c21bcecceda1000000", // 1_000_000*10**18
			"name":    fmt.Sprintf("ibc-%d", index),
		}
		if err := dyno.Append(g, genesisAccount, "accounts"); err != nil {
			return fmt.Errorf("failed to set add genesis accounts in genesis json: %w", err)
		}
	}
	result, _ := json.Marshal(g)

	header := map[string]string{
		"name": "genesis.json",
	}
	err = in.CopyFileToContainer(context.WithValue(ctx, "file-header", header), result, containerID, "/goloop/data/")
	return err
}

func (in *IconNode) HostName() string {
	return dockerutil.CondenseHostName(in.Name())
}

func (in *IconNode) Bind() []string {
	return []string{fmt.Sprintf("%s:%s", in.VolumeName, in.HomeDir())}
}

func (in *IconNode) HomeDir() string {
	return path.Join("/var/icon-chain", in.Chain.Config().Name)
}

func (in *IconNode) StartContainer(ctx context.Context) error {
	if err := dockerutil.StartContainer(ctx, in.DockerClient, in.ContainerID); err != nil {
		return err
	}

	c, err := in.DockerClient.ContainerInspect(ctx, in.ContainerID)
	if err != nil {
		return err
	}
	in.HostRPCPort = dockerutil.GetHostPort(c, rpcPort)
	in.logger().Info("Icon chain node started", zap.String("container", in.Name()), zap.String("rpc_port", in.HostRPCPort))

	uri := "http://" + in.HostRPCPort + "/api/v3/"
	var l iconlog.Logger
	in.Client = *iconclient.NewClient(uri, l)
	return nil
}

func (in *IconNode) logger() *zap.Logger {
	return in.log.With(
		zap.String("chain_id", in.Chain.Config().ChainID),
		zap.String("test", in.TestName),
	)
}

func (in *IconNode) Exec(ctx context.Context, cmd []string, env []string) ([]byte, []byte, error) {
	job := dockerutil.NewImage(in.logger(), in.DockerClient, in.NetworkID, in.TestName, chains.GetEnvOrDefault(GOLOOP_IMAGE_ENV, GOLOOP_IMAGE), chains.GetEnvOrDefault(GOLOOP_IMAGE_TAG_ENV, GOLOOP_IMAGE_TAG))
	opts := dockerutil.ContainerOptions{
		Env:   env,
		Binds: in.Bind(),
	}
	res := job.Run(ctx, cmd, opts)
	return res.Stdout, res.Stderr, res.Err
}

func (in *IconNode) BinCommand(command ...string) []string {
	command = append([]string{in.Chain.Config().Bin}, command...)
	return command
}

func (in *IconNode) ExecBin(ctx context.Context, command ...string) ([]byte, []byte, error) {
	return in.Exec(ctx, in.BinCommand(command...), nil)
}

func (in *IconNode) GetBlockByHeight(ctx context.Context, height int64) (string, error) {
	in.lock.Lock()
	defer in.lock.Unlock()
	uri := "http://" + in.HostRPCPort + "/api/v3"
	block, _, err := in.ExecBin(ctx,
		"rpc", "blockbyheight", fmt.Sprint(height),
		"--uri", uri,
	)
	return string(block), err
}

func (in *IconNode) FindTxs(ctx context.Context, height uint64) ([]blockdb.Tx, error) {
	var flag = true
	if flag {
		time.Sleep(3 * time.Second)
		flag = false
	}

	time.Sleep(2 * time.Second)
	blockHeight := icontypes.BlockHeightParam{Height: icontypes.NewHexInt(int64(height))}
	res, err := in.Client.GetBlockByHeight(&blockHeight)
	if err != nil {
		return make([]blockdb.Tx, 0, 0), nil
	}
	txs := make([]blockdb.Tx, 0, len(res.NormalTransactions)+2)
	var newTx blockdb.Tx
	for _, tx := range res.NormalTransactions {
		newTx.Data = []byte(fmt.Sprintf(`{"data":"%s"}`, tx.Data))
	}

	// ToDo Add events from block if any to newTx.Events.
	// Event is an alternative representation of tendermint/abci/types.Event
	return txs, nil
}

func (in *IconNode) Height(ctx context.Context) (uint64, error) {
	res, err := in.Client.GetLastBlock()
	return uint64(res.Height), err
}

func (in *IconNode) GetBalance(ctx context.Context, address string) (int64, error) {
	addr := icontypes.AddressParam{Address: icontypes.Address(address)}
	bal, err := in.Client.GetBalance(&addr)
	return bal.Int64(), err
}

func (in *IconNode) DeployContract(ctx context.Context, scorePath, keystorePath, initMessage string) (string, error) {
	// Write Contract file to Docker volume
	_, score := filepath.Split(scorePath)
	if err := in.CopyFile(ctx, scorePath, score); err != nil {
		return "", fmt.Errorf("error copying keystore to Docker volume: %w", err)
	}

	// Deploy the contract
	hash, err := in.ExecTx(ctx, initMessage, path.Join(in.HomeDir(), score), keystorePath)
	if err != nil {
		return "", err
	}

	//wait for few blocks
	time.Sleep(3 * time.Second)

	// Get Score Address
	trResult, err := in.TransactionResult(ctx, hash)

	if err != nil {
		return "", err
	}
	return string(trResult.SCOREAddress), nil

}

// Get Transaction result when hash is provided after executing a transaction
func (in *IconNode) TransactionResult(ctx context.Context, hash string) (*icontypes.TransactionResult, error) {
	uri := fmt.Sprintf("http://%s:9080/api/v3", in.Name()) //"http://" + in.HostRPCPort + "/api/v3"
	out, _, err := in.ExecBin(ctx, "rpc", "txresult", hash, "--uri", uri)
	if err != nil {
		return nil, err
	}
	var result = new(icontypes.TransactionResult)
	return result, json.Unmarshal(out, result)
}

// ExecTx executes a transaction, waits for 2 blocks if successful, then returns the tx hash.
func (in *IconNode) ExecTx(ctx context.Context, initMessage string, filePath string, keystorePath string, command ...string) (string, error) {
	var output string
	in.lock.Lock()
	defer in.lock.Unlock()
	stdout, _, err := in.Exec(ctx, in.TxCommand(ctx, initMessage, filePath, keystorePath, command...), nil)
	if err != nil {
		return "", err
	}
	return output, json.Unmarshal(stdout, &output)
}

// TxCommand is a helper to retrieve a full command for broadcasting a tx
// with the chain node binary.
func (in *IconNode) TxCommand(ctx context.Context, initMessage, filePath, keystorePath string, command ...string) []string {
	// get password from pathname as pathname will have the password prefixed. ex - Alice.Json
	_, key := filepath.Split(keystorePath)
	fileName := strings.Split(key, ".")
	password := fileName[0]

	command = append([]string{"rpc", "sendtx", "deploy", filePath}, command...)
	command = append(command,
		"--key_store", keystorePath,
		"--key_password", password,
		"--step_limit", "5000000000",
		"--content_type", "application/java",
	)
	if initMessage != "" && initMessage != "{}" {
		if strings.HasPrefix(initMessage, "{") {
			command = append(command, "--params", initMessage)
		} else {
			command = append(command, "--param", initMessage)
		}
	}

	return in.NodeCommand(command...)
}

// NodeCommand is a helper to retrieve a full command for a chain node binary.
// when interactions with the RPC endpoint are necessary.
// For example, if chain node binary is `gaiad`, and desired command is `gaiad keys show key1`,
// pass ("keys", "show", "key1") for command to return the full command.
// Will include additional flags for node URL, home directory, and chain ID.
func (in *IconNode) NodeCommand(command ...string) []string {
	command = in.BinCommand(command...)
	return append(command,
		"--uri", fmt.Sprintf("http://%s:9080/api/v3", in.Name()), //fmt.Sprintf("http://%s/api/v3", in.HostRPCPort),
		"--nid", "0x3",
	)
}

// CopyFile adds a file from the host filesystem to the docker filesystem
// relPath describes the location of the file in the docker volume relative to
// the home directory
func (tn *IconNode) CopyFile(ctx context.Context, srcPath, dstPath string) error {
	content, err := os.ReadFile(srcPath)
	if err != nil {
		return err
	}
	return tn.WriteFile(ctx, content, dstPath)
}

// WriteFile accepts file contents in a byte slice and writes the contents to
// the docker filesystem. relPath describes the location of the file in the
// docker volume relative to the home directory
func (tn *IconNode) WriteFile(ctx context.Context, content []byte, relPath string) error {
	fw := dockerutil.NewFileWriter(tn.logger(), tn.DockerClient, tn.TestName)
	return fw.WriteFile(ctx, tn.VolumeName, relPath, content)
}

func (in *IconNode) QueryContract(ctx context.Context, scoreAddress, methodName, params string) ([]byte, error) {
	uri := fmt.Sprintf("http://%s:9080/api/v3", in.Name())
	var args = []string{"rpc", "call", "--to", scoreAddress, "--method", methodName, "--uri", uri}
	if params != "" {
		var paramName = "--param"
		if strings.HasPrefix(params, "{") && strings.HasSuffix(params, "}") {
			paramName = "--raw"
		}
		args = append(args, paramName, params)
	}
	out, _, err := in.ExecBin(ctx, args...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (in *IconNode) RestoreKeystore(ctx context.Context, ks []byte, keyName string) error {
	return in.WriteFile(ctx, ks, keyName+".json")
}

func (in *IconNode) ExecuteContract(ctx context.Context, scoreAddress, methodName, keyStorePath, params string) (string, error) {
	return in.ExecCallTx(ctx, scoreAddress, methodName, keyStorePath, params)
}

func (in *IconNode) ExecCallTx(ctx context.Context, scoreAddress, methodName, keystorePath, params string) (string, error) {
	var output string
	in.lock.Lock()
	defer in.lock.Unlock()
	stdout, _, err := in.Exec(ctx, in.ExecCallTxCommand(ctx, scoreAddress, methodName, keystorePath, params), nil)
	if err != nil {
		return "", err
	}
	return output, json.Unmarshal(stdout, &output)
}

func (in *IconNode) ExecCallTxCommand(ctx context.Context, scoreAddress, methodName, keystorePath, params string) []string {
	// get password from pathname as pathname will have the password prefixed. ex - Alice.Json
	_, key := filepath.Split(keystorePath)
	fileName := strings.Split(key, ".")
	password := fileName[0]
	command := []string{"rpc", "sendtx", "call"}

	command = append(command,
		"--to", scoreAddress,
		"--method", methodName,
		"--key_store", keystorePath,
		"--key_password", password,
		"--step_limit", "5000000000",
	)

	if params != "" && params != "{}" {
		if strings.HasPrefix(params, "{") {
			command = append(command, "--params", params)
		} else {
			command = append(command, "--param", params)
		}
	}

	if methodName == "registerPRep" {
		command = append(command, "--value", "2000000000000000000000")
	}

	return in.NodeCommand(command...)
}

func (in *IconNode) GetDebugTrace(ctx context.Context, hash icontypes.HexBytes) (*DebugTrace, error) {
	uri := fmt.Sprintf("http://%s:9080/api/v3d", in.Name())
	out, _, err := in.ExecBin(ctx, "debug", "trace", string(hash), "--uri", uri)
	if err != nil {
		return nil, err
	}
	var result = new(DebugTrace)
	return result, json.Unmarshal(out, result)

}
