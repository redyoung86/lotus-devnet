package ldevnet

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"time"

	miner2 "github.com/filecoin-project/lotus/miner"
	"github.com/filecoin-project/lotus/storage/mockstorage"

	"github.com/filecoin-project/go-storedcounter"
	"github.com/filecoin-project/lotus/api/apistruct"
	sealing "github.com/filecoin-project/lotus/extern/storage-sealing"
	"github.com/ipfs/go-datastore"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/peer"
	mocknet "github.com/libp2p/go-libp2p/p2p/net/mock"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/specs-actors/actors/builtin"
	"github.com/filecoin-project/specs-actors/actors/builtin/miner"
	"github.com/filecoin-project/specs-actors/actors/builtin/power"
	"github.com/filecoin-project/specs-actors/actors/builtin/verifreg"

	"github.com/filecoin-project/go-jsonrpc"
	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/api/client"
	"github.com/filecoin-project/lotus/api/test"
	"github.com/filecoin-project/lotus/build"
	"github.com/filecoin-project/lotus/chain/actors"
	"github.com/filecoin-project/lotus/chain/gen"
	genesis2 "github.com/filecoin-project/lotus/chain/gen/genesis"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/chain/wallet"
	sectorstorage "github.com/filecoin-project/lotus/extern/sector-storage"
	"github.com/filecoin-project/lotus/extern/sector-storage/ffiwrapper"
	"github.com/filecoin-project/lotus/extern/sector-storage/mock"
	"github.com/filecoin-project/lotus/genesis"
	"github.com/filecoin-project/lotus/node"
	"github.com/filecoin-project/lotus/node/modules"
	"github.com/filecoin-project/lotus/node/modules/dtypes"
	testing2 "github.com/filecoin-project/lotus/node/modules/testing"
	"github.com/filecoin-project/lotus/node/repo"
)

const (
	DefaultDurationMs = 100
	DefaultDuration   = time.Millisecond * DefaultDurationMs
)

func init() {
	power.ConsensusMinerMinPower = big.NewInt(2048)
	miner.SupportedProofTypes = map[abi.RegisteredSealProof]struct{}{
		abi.RegisteredSealProof_StackedDrg2KiBV1: {},
	}
	verifreg.MinVerifiedDealSize = big.NewInt(256)
	os.Setenv("TRUST_PARAMS", "1")
	os.Setenv("BELLMAN_NO_GPU", "1")
}

type LocalDevnet struct {
	Client *apistruct.FullNodeStruct

	numMiners int
	cancel    context.CancelFunc
	closer    func()
	done      chan struct{}
}

func (ld *LocalDevnet) Close() {
	ld.cancel()

	for i := 0; i < ld.numMiners; i++ {
		<-ld.done
	}
	close(ld.done)
	ld.closer()
}

var PresealGenesis = -1

func New(numMiners int, blockDur time.Duration, bigSector bool, ipfsAddr string) (*LocalDevnet, error) {
	if bigSector {
		miner.SupportedProofTypes = map[abi.RegisteredSealProof]struct{}{
			abi.RegisteredSealProof_StackedDrg512MiBV1: {},
		}
	}
	miners := make([]test.StorageMiner, numMiners)
	for i := 0; i < numMiners; i++ {
		miners[i] = test.StorageMiner{Full: 0, Preseal: PresealGenesis}
	}
	n, sn, closer, err := rpcBuilder(1, miners, bigSector, ipfsAddr)
	if err != nil {
		return nil, err
	}
	client := n[0].FullNode.(*apistruct.FullNodeStruct)
	ctx, cancel := context.WithCancel(context.Background())
	addrinfo, err := client.NetAddrsListen(ctx)
	if err != nil {
		cancel()
		return nil, err
	}

	done := make(chan struct{})
	for i := range miners {
		if err := sn[i].NetConnect(ctx, addrinfo); err != nil {
			cancel()
			return nil, err
		}
		time.Sleep(time.Second)
	}
	var mineNext = miner2.MineReq{
		InjectNulls: 0,
		Done:        func(bool, abi.ChainEpoch, error) {},
	}
	go func() {
		i := 0
		mine := true
		defer func() { done <- struct{}{} }()
		for mine {
			time.Sleep(blockDur)
			if ctx.Err() != nil {
				panic("context cancelled")
				mine = false
				continue
			}
			if err := sn[i].MineOne(context.Background(), mineNext); err != nil {
				panic(err)
			}

			i = (i + 1) % len(miners)
		}
	}()

	go func() {
		for {
			for i := 0; i < numMiners; i++ {
				snums, err := sn[i].SectorsList(ctx)
				if err != nil {
					panic(err)
				}
				for _, snum := range snums {
					si, err := sn[i].SectorsStatus(ctx, snum, false)
					if err != nil {
						panic(err)
					}

					fmt.Printf("######## STATE: %s\n", si.State)
					if si.State == api.SectorState(sealing.WaitDeals) {
						if err := sn[i].SectorStartSealing(ctx, snum); err != nil {
							panic(err)

						}
					}
				}
			}
			time.Sleep(time.Second)
		}
	}()

	for i := range miners {
		for j := range miners {
			if j == i {
				continue
			}
			mainfo, err := sn[j].NetAddrsListen(ctx)
			if err != nil {
				cancel()
				return nil, err
			}
			if err := sn[i].NetConnect(ctx, mainfo); err != nil {
				cancel()
				return nil, err
			}
		}
	}

	time.Sleep(blockDur * 2) // Give time to mine at least 1 block
	return &LocalDevnet{
		Client:    client,
		closer:    closer,
		cancel:    cancel,
		done:      done,
		numMiners: numMiners,
	}, nil
}

func rpcBuilder(nFull int, storage []test.StorageMiner, bigSector bool, ipfsAddr string) ([]test.TestNode, []test.TestStorageNode, func(), error) {
	fullApis, storaApis, err := mockSbBuilder(nFull, storage, bigSector, ipfsAddr)
	if err != nil {
		return nil, nil, nil, err
	}
	fulls := make([]test.TestNode, nFull)
	storers := make([]test.TestStorageNode, len(storage))

	var closers []func()
	for i, a := range fullApis {
		rpcServer := jsonrpc.NewServer()
		rpcServer.Register("Filecoin", a)
		go func() {
			if err := http.ListenAndServe(":7777", rpcServer); err != nil {
				if err != http.ErrServerClosed {
					panic(err)
				}
			}
		}()
		var err error
		time.Sleep(time.Second)
		fulls[i].FullNode, _, err = client.NewFullNodeRPC(context.Background(), "ws://127.0.0.1:7777", nil)
		if err != nil {
			return nil, nil, nil, err
		}
	}

	for i, a := range storaApis {
		rpcServer := jsonrpc.NewServer()
		rpcServer.Register("Filecoin", a)
		testServ := httptest.NewServer(rpcServer)
		closers = append(closers, func() { testServ.Close() })

		var err error
		storers[i].StorageMiner, _, err = client.NewStorageMinerRPC(context.Background(), "ws://"+testServ.Listener.Addr().String(), nil)
		if err != nil {
			return nil, nil, nil, err
		}
		storers[i].MineOne = a.MineOne
	}

	return fulls, storers, func() {
		for _, c := range closers {
			c()
		}
	}, nil
}

const nGenesisPreseals = 2

func mockSbBuilder(nFull int, storage []test.StorageMiner, bigSector bool, ipfsAddr string) ([]test.TestNode, []test.TestStorageNode, error) {
	ctx := context.Background()
	mn := mocknet.New(ctx)

	fulls := make([]test.TestNode, nFull)
	storers := make([]test.TestStorageNode, len(storage))

	var minersPk []crypto.PrivKey
	var minersPid []peer.ID
	for i := 0; i < len(storage); i++ {
		pk, _, err := crypto.GenerateEd25519Key(rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		pid, err := peer.IDFromPrivateKey(pk)
		if err != nil {
			return nil, nil, err
		}
		minersPk = append(minersPk, pk)
		minersPid = append(minersPid, pid)
	}

	var genbuf bytes.Buffer

	// PRESEAL SECTION, TRY TO REPLACE WITH BETTER IN THE FUTURE
	// TODO: would be great if there was a better way to fake the preseals
	var genms []genesis.Miner
	var genaccs []genesis.Actor
	var maddrs []address.Address
	var keys []*wallet.Key
	var pidKeys []crypto.PrivKey
	for i := 0; i < len(storage); i++ {
		maddr, err := address.NewIDAddress(genesis2.MinerStart + uint64(i))
		if err != nil {
			return nil, nil, err
		}
		preseals := storage[i].Preseal
		if preseals == test.PresealGenesis {
			preseals = nGenesisPreseals
		}

		size := abi.SectorSize(2048)
		if bigSector {
			size = abi.SectorSize(1024 * 1024 * 512)
		}
		genm, k, err := mockstorage.PreSeal(size, maddr, preseals)
		if err != nil {
			return nil, nil, err
		}
		genm.PeerId = minersPid[i]

		wk, err := wallet.NewKey(*k)
		if err != nil {
			return nil, nil, err
		}

		genaccs = append(genaccs, genesis.Actor{
			Type:    genesis.TAccount,
			Balance: big.Mul(big.NewInt(400000000), types.NewInt(build.FilecoinPrecision)),
			Meta:    (&genesis.AccountMeta{Owner: wk.Address}).ActorMeta(),
		})

		keys = append(keys, wk)
		pidKeys = append(pidKeys, minersPk[i])
		maddrs = append(maddrs, maddr)
		genms = append(genms, *genm)
	}
	templ := &genesis.Template{
		Accounts:         genaccs,
		Miners:           genms,
		Timestamp:        uint64(time.Now().Add(-time.Hour * 100000).Unix()),
		VerifregRootKey:  gen.DefaultVerifregRootkeyActor,
		RemainderAccount: gen.DefaultRemainderAccountActor,
	}

	// END PRESEAL SECTION

	for i := 0; i < nFull; i++ {
		var genesis node.Option
		if i == 0 {
			genesis = node.Override(new(modules.Genesis), testing2.MakeGenesisMem(&genbuf, *templ))
		} else {
			genesis = node.Override(new(modules.Genesis), modules.LoadGenesis(genbuf.Bytes()))
		}

		var err error
		_, err = node.New(ctx,
			node.FullAPI(&fulls[i].FullNode),
			node.Online(),
			node.Repo(repo.NewMemory(nil)),
			node.MockHost(mn),
			node.Test(),

			node.Override(new(ffiwrapper.Verifier), mock.MockVerifier),

			genesis,
			node.ApplyIf(func(s *node.Settings) bool { return len(ipfsAddr) > 0 },
				node.Override(new(dtypes.ClientBlockstore), modules.IpfsClientBlockstore(ipfsAddr)),
				node.Override(new(dtypes.ClientRetrievalStoreManager), modules.ClientBlockstoreRetrievalStoreManager),
			),
		)
		if err != nil {
			return nil, nil, err
		}
	}

	for i, def := range storage {
		minerID := abi.ActorID(genesis2.MinerStart + uint64(i))
		if def.Full != 0 {
			return nil, nil, fmt.Errorf("storage nodes only supported on the first full node")
		}

		f := fulls[def.Full]
		if _, err := f.FullNode.WalletImport(ctx, &keys[i].KeyInfo); err != nil {
			return nil, nil, err
		}
		if err := f.FullNode.WalletSetDefault(ctx, keys[i].Address); err != nil {
			return nil, nil, err
		}

		sectors := make([]abi.SectorID, len(genms[i].Sectors))
		for i, sector := range genms[i].Sectors {
			sectors[i] = abi.SectorID{
				Miner:  minerID,
				Number: sector.SectorID,
			}
		}

		storers[i] = testStorageNode(ctx, genms[i].Worker, maddrs[i], minersPk[i], f, mn, node.Options(
			node.Override(new(sectorstorage.SectorManager), func() (sectorstorage.SectorManager, error) {
				return mock.NewMockSectorMgr(build.DefaultSectorSize(), sectors), nil
			}),
			node.Override(new(ffiwrapper.Verifier), mock.MockVerifier),
			node.Unset(new(*sectorstorage.Manager)),
		))
	}

	if err := mn.LinkAll(); err != nil {
		return nil, nil, err
	}

	if len(storers) > 0 {
		// Mine 2 blocks to setup some CE stuff in some actors
		var wait sync.Mutex
		wait.Lock()

		MineUntilBlock(ctx, fulls[0], storers[0], func(abi.ChainEpoch) {
			wait.Unlock()
		})
		wait.Lock()
		MineUntilBlock(ctx, fulls[0], storers[0], func(abi.ChainEpoch) {
			wait.Unlock()
		})
		wait.Lock()
	}

	return fulls, storers, nil
}

func MineUntilBlock(ctx context.Context, fn test.TestNode, sn test.TestStorageNode, cb func(abi.ChainEpoch)) {
	for i := 0; i < 1000; i++ {
		var success bool
		var err error
		var epoch abi.ChainEpoch
		wait := make(chan struct{})
		mineErr := sn.MineOne(ctx, miner2.MineReq{
			Done: func(win bool, ep abi.ChainEpoch, e error) {
				success = win
				err = e
				epoch = ep
				wait <- struct{}{}
			},
		})
		if mineErr != nil {
			panic(mineErr)
		}
		<-wait
		if err != nil {
			panic(err)
		}
		if success {
			// Wait until it shows up on the given full nodes ChainHead
			nloops := 50
			for i := 0; i < nloops; i++ {
				ts, err := fn.ChainHead(ctx)
				if err != nil {
					panic(err)
				}
				if ts.Height() == epoch {
					break
				}
				if i == nloops-1 {
					panic(err)
				}
				time.Sleep(time.Millisecond * 10)
			}

			if cb != nil {
				cb(epoch)
			}
			return
		}
		fmt.Printf("did not mine block, trying again %v", i)
	}
	panic("failed to mine 1000 times in a row...")
}

func testStorageNode(ctx context.Context, waddr address.Address, act address.Address, pk crypto.PrivKey, tnd test.TestNode, mn mocknet.Mocknet, opts node.Option) test.TestStorageNode {
	r := repo.NewMemory(nil)

	lr, err := r.Lock(repo.StorageMiner)
	if err != nil {
		panic(err)
	}
	ks, err := lr.KeyStore()
	if err != nil {
		panic(err)
	}
	kbytes, err := pk.Bytes()
	if err != nil {
		panic(err)
	}
	err = ks.Put("libp2p-host", types.KeyInfo{
		Type:       "libp2p-host",
		PrivateKey: kbytes,
	})
	if err != nil {
		panic(err)
	}
	ds, err := lr.Datastore("/metadata")
	if err != nil {
		panic(err)
	}
	err = ds.Put(datastore.NewKey("miner-address"), act.Bytes())
	if err != nil {
		panic(err)
	}
	nic := storedcounter.New(ds, datastore.NewKey(modules.StorageCounterDSPrefix))
	for i := 0; i < nGenesisPreseals; i++ {
		_, err := nic.Next()
		if err != nil {
			panic(err)
		}
	}
	_, err = nic.Next()
	if err != nil {
		panic(err)
	}

	err = lr.Close()
	if err != nil {
		panic(err)
	}
	peerid, err := peer.IDFromPrivateKey(pk)
	if err != nil {
		panic(err)
	}
	enc, err := actors.SerializeParams(&miner.ChangePeerIDParams{NewID: abi.PeerID(peerid)})
	if err != nil {
		panic(err)
	}
	msg := &types.Message{
		To:     act,
		From:   waddr,
		Method: builtin.MethodsMiner.ChangePeerID,
		Params: enc,
		Value:  types.NewInt(0),
	}

	_, err = tnd.MpoolPushMessage(ctx, msg, nil)
	if err != nil {
		panic(err)
	}
	// start node
	var minerapi api.StorageMiner

	mineBlock := make(chan miner2.MineReq)
	// TODO: use stop
	_, err = node.New(ctx,
		node.StorageMiner(&minerapi),
		node.Online(),
		node.Repo(r),
		node.Test(),

		node.MockHost(mn),

		node.Override(new(api.FullNode), tnd),
		node.Override(new(*miner2.Miner), miner2.NewTestMiner(mineBlock, act)),

		opts,
	)
	if err != nil {
		panic(err)
	}

	/*// Bootstrap with full node
	remoteAddrs, err := tnd.NetAddrsListen(ctx)
	require.NoError(t, err)

	err = minerapi.NetConnect(ctx, remoteAddrs)
	require.NoError(t, err)*/
	mineOne := func(ctx context.Context, req miner2.MineReq) error {
		select {
		case mineBlock <- req:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return test.TestStorageNode{StorageMiner: minerapi, MineOne: mineOne}
}
