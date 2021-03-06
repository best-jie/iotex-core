// Copyright (c) 2018 IoTeX
// This is an alpha (internal) release and is not suitable for production. This source code is provided 'as is' and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package chainservice

import (
	"context"
	"os"

	"github.com/golang/protobuf/proto"
	peerstore "github.com/libp2p/go-libp2p-peerstore"
	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/iotexproject/iotex-core/action"
	"github.com/iotexproject/iotex-core/action/protocol"
	"github.com/iotexproject/iotex-core/actpool"
	"github.com/iotexproject/iotex-core/address"
	"github.com/iotexproject/iotex-core/api"
	"github.com/iotexproject/iotex-core/blockchain"
	"github.com/iotexproject/iotex-core/blockchain/block"
	"github.com/iotexproject/iotex-core/blockchain/genesis"
	"github.com/iotexproject/iotex-core/blocksync"
	"github.com/iotexproject/iotex-core/config"
	"github.com/iotexproject/iotex-core/consensus"
	"github.com/iotexproject/iotex-core/dispatcher"
	"github.com/iotexproject/iotex-core/explorer"
	explorerapi "github.com/iotexproject/iotex-core/explorer/idl/explorer"
	"github.com/iotexproject/iotex-core/indexservice"
	"github.com/iotexproject/iotex-core/p2p"
	"github.com/iotexproject/iotex-core/pkg/keypair"
	"github.com/iotexproject/iotex-core/pkg/log"
	"github.com/iotexproject/iotex-core/protogen/iotexrpc"
	"github.com/iotexproject/iotex-core/protogen/iotextypes"
)

// ChainService is a blockchain service with all blockchain components.
type ChainService struct {
	actpool      actpool.ActPool
	blocksync    blocksync.BlockSync
	consensus    consensus.Consensus
	chain        blockchain.Blockchain
	explorer     *explorer.Server
	api          *api.Server
	indexBuilder *blockchain.IndexBuilder
	indexservice *indexservice.Server
	registry     *protocol.Registry
}

type optionParams struct {
	rootChainAPI  explorerapi.Explorer
	isTesting     bool
	genesisConfig genesis.Genesis
}

// Option sets ChainService construction parameter.
type Option func(ops *optionParams) error

// WithRootChainAPI is an option to add a root chain api to ChainService.
func WithRootChainAPI(exp explorerapi.Explorer) Option {
	return func(ops *optionParams) error {
		ops.rootChainAPI = exp
		return nil
	}
}

// WithTesting is an option to create a testing ChainService.
func WithTesting() Option {
	return func(ops *optionParams) error {
		ops.isTesting = true
		return nil
	}
}

// WithGenesis is an option to set genesis config
func WithGenesis(genesisConfig genesis.Genesis) Option {
	return func(ops *optionParams) error {
		ops.genesisConfig = genesisConfig
		return nil
	}
}

// New creates a ChainService from config and network.Overlay and dispatcher.Dispatcher.
func New(
	cfg config.Config,
	p2pAgent *p2p.Agent,
	dispatcher dispatcher.Dispatcher,
	opts ...Option,
) (*ChainService, error) {
	var ops optionParams
	for _, opt := range opts {
		if err := opt(&ops); err != nil {
			return nil, err
		}
	}

	var chainOpts []blockchain.Option
	if ops.isTesting {
		chainOpts = []blockchain.Option{
			blockchain.InMemStateFactoryOption(),
			blockchain.InMemDaoOption(),
		}
	} else {
		chainOpts = []blockchain.Option{
			blockchain.DefaultStateFactoryOption(),
			blockchain.BoltDBDaoOption(),
		}
	}
	registry := protocol.Registry{}
	chainOpts = append(chainOpts, blockchain.GenesisOption(ops.genesisConfig), blockchain.RegistryOption(&registry))

	// create Blockchain
	chain := blockchain.NewBlockchain(cfg, chainOpts...)
	if chain == nil && cfg.Chain.EnableFallBackToFreshDB {
		log.L().Warn("Chain db and trie db are falling back to fresh ones.")
		if err := os.Rename(cfg.Chain.ChainDBPath, cfg.Chain.ChainDBPath+".old"); err != nil {
			return nil, errors.Wrap(err, "failed to rename old chain db")
		}
		if err := os.Rename(cfg.Chain.TrieDBPath, cfg.Chain.TrieDBPath+".old"); err != nil {
			return nil, errors.Wrap(err, "failed to rename old trie db")
		}
		chain = blockchain.NewBlockchain(cfg, blockchain.DefaultStateFactoryOption(), blockchain.BoltDBDaoOption())
	}

	var indexBuilder *blockchain.IndexBuilder
	var err error
	if cfg.Chain.EnableIndex && cfg.Chain.EnableAsyncIndexWrite {
		if indexBuilder, err = blockchain.NewIndexBuilder(chain); err != nil {
			return nil, errors.Wrap(err, "failed to create index builder")
		}
		if err := chain.AddSubscriber(indexBuilder); err != nil {
			log.L().Warn("Failed to add subscriber: index builder.", zap.Error(err))
		}
	}

	// Create ActPool
	actPool, err := actpool.NewActPool(chain, cfg.ActPool)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create actpool")
	}

	copts := []consensus.Option{
		consensus.WithBroadcast(func(msg proto.Message) error {
			return p2pAgent.BroadcastOutbound(p2p.WitContext(context.Background(), p2p.Context{ChainID: chain.ChainID()}), msg)
		}),
	}
	if ops.rootChainAPI != nil {
		copts = append(copts, consensus.WithRootChainAPI(ops.rootChainAPI))
	}
	consensus, err := consensus.NewConsensus(cfg, chain, actPool, copts...)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create consensus")
	}
	bs, err := blocksync.NewBlockSyncer(
		cfg,
		chain,
		actPool,
		consensus,
		blocksync.WithUnicastOutBound(func(ctx context.Context, peer peerstore.PeerInfo, msg proto.Message) error {
			ctx = p2p.WitContext(ctx, p2p.Context{ChainID: chain.ChainID()})
			return p2pAgent.UnicastOutbound(ctx, peer, msg)
		}),
		blocksync.WithNeighbors(p2pAgent.Neighbors),
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create blockSyncer")
	}
	var idx *indexservice.Server
	if cfg.Indexer.Enabled {
		idx = indexservice.NewServer(cfg, chain)
		if idx == nil {
			return nil, errors.Wrap(err, "failed to create index service")
		}
	}

	var exp *explorer.Server
	if cfg.Explorer.Enabled {
		exp, err = explorer.NewServer(
			cfg.Explorer,
			chain,
			consensus,
			dispatcher,
			actPool,
			idx,
			explorer.WithBroadcastOutbound(func(ctx context.Context, chainID uint32, msg proto.Message) error {
				ctx = p2p.WitContext(ctx, p2p.Context{ChainID: chainID})
				return p2pAgent.BroadcastOutbound(ctx, msg)
			}),
			explorer.WithNeighbors(p2pAgent.Neighbors),
			explorer.WithNetworkInfo(p2pAgent.Info),
		)
		if err != nil {
			return nil, err
		}
	}

	var apiSvr *api.Server
	if cfg.API.Enabled {
		apiSvr, err = api.NewServer(
			cfg.API,
			chain,
			dispatcher,
			actPool,
			idx,
			api.WithBroadcastOutbound(func(ctx context.Context, chainID uint32, msg proto.Message) error {
				ctx = p2p.WitContext(ctx, p2p.Context{ChainID: chainID})
				return p2pAgent.BroadcastOutbound(ctx, msg)
			}),
		)
		if err != nil {
			return nil, err
		}
	}

	return &ChainService{
		actpool:      actPool,
		chain:        chain,
		blocksync:    bs,
		consensus:    consensus,
		indexservice: idx,
		indexBuilder: indexBuilder,
		explorer:     exp,
		api:          apiSvr,
		registry:     &registry,
	}, nil
}

// Start starts the server
func (cs *ChainService) Start(ctx context.Context) error {
	if cs.indexservice != nil {
		if err := cs.indexservice.Start(ctx); err != nil {
			return errors.Wrap(err, "error when starting indexservice")
		}
	}
	if err := cs.chain.Start(ctx); err != nil {
		return errors.Wrap(err, "error when starting blockchain")
	}
	if err := cs.consensus.Start(ctx); err != nil {
		return errors.Wrap(err, "error when starting consensus")
	}
	if err := cs.blocksync.Start(ctx); err != nil {
		return errors.Wrap(err, "error when starting blocksync")
	}
	if cs.explorer != nil {
		if err := cs.explorer.Start(ctx); err != nil {
			return errors.Wrap(err, "error when starting explorer")
		}
	}
	if cs.api != nil {
		if err := cs.api.Start(); err != nil {
			return errors.Wrap(err, "err when starting API server")
		}
	}
	if cs.indexBuilder != nil {
		if err := cs.indexBuilder.Start(ctx); err != nil {
			return errors.Wrap(err, "error when starting index builder")
		}
	}
	return nil
}

// Stop stops the server
func (cs *ChainService) Stop(ctx context.Context) error {
	if cs.indexBuilder != nil {
		if err := cs.indexBuilder.Stop(ctx); err != nil {
			return errors.Wrap(err, "error when stopping index builder")
		}
	}
	if cs.explorer != nil {
		if err := cs.explorer.Stop(ctx); err != nil {
			return errors.Wrap(err, "error when stopping explorer")
		}
	}
	if cs.api != nil {
		if err := cs.api.Stop(); err != nil {
			return errors.Wrap(err, "error when stopping API server")
		}
	}
	if cs.indexservice != nil {
		if err := cs.indexservice.Stop(ctx); err != nil {
			return errors.Wrap(err, "error when stopping indexservice")
		}
	}
	if err := cs.consensus.Stop(ctx); err != nil {
		return errors.Wrap(err, "error when stopping consensus")
	}
	if err := cs.blocksync.Stop(ctx); err != nil {
		return errors.Wrap(err, "error when stopping blocksync")
	}
	if err := cs.chain.Stop(ctx); err != nil {
		return errors.Wrap(err, "error when stopping blockchain")
	}
	return nil
}

// HandleAction handles incoming action request.
func (cs *ChainService) HandleAction(_ context.Context, actPb *iotextypes.Action) error {
	var act action.SealedEnvelope
	if err := act.LoadProto(actPb); err != nil {
		return err
	}
	if err := cs.actpool.Add(act); err != nil {
		callerPKHash := keypair.HashPubKey(act.SrcPubkey())
		callerAddr, err := address.FromBytes(callerPKHash[:])
		if err != nil {
			return err
		}
		log.L().Debug("Failed to add action.",
			zap.Error(err),
			zap.String("src", callerAddr.String()),
			zap.Uint64("nonce", act.Nonce()))
		return err
	}
	return nil
}

// HandleBlock handles incoming block request.
func (cs *ChainService) HandleBlock(ctx context.Context, pbBlock *iotextypes.Block) error {
	blk := &block.Block{}
	if err := blk.ConvertFromBlockPb(pbBlock); err != nil {
		return err
	}
	return cs.blocksync.ProcessBlock(ctx, blk)
}

// HandleBlockSync handles incoming block sync request.
func (cs *ChainService) HandleBlockSync(ctx context.Context, pbBlock *iotextypes.Block) error {
	blk := &block.Block{}
	if err := blk.ConvertFromBlockPb(pbBlock); err != nil {
		return err
	}
	return cs.blocksync.ProcessBlockSync(ctx, blk)
}

// HandleSyncRequest handles incoming sync request.
func (cs *ChainService) HandleSyncRequest(ctx context.Context, peer peerstore.PeerInfo, sync *iotexrpc.BlockSync) error {
	return cs.blocksync.ProcessSyncRequest(ctx, peer, sync)
}

// HandleConsensusMsg handles incoming consensus message.
func (cs *ChainService) HandleConsensusMsg(msg *iotexrpc.Consensus) error {
	return cs.consensus.HandleConsensusMsg(msg)
}

// ChainID returns ChainID.
func (cs *ChainService) ChainID() uint32 { return cs.chain.ChainID() }

// Blockchain returns the Blockchain
func (cs *ChainService) Blockchain() blockchain.Blockchain {
	return cs.chain
}

// ActionPool returns the Action pool
func (cs *ChainService) ActionPool() actpool.ActPool {
	return cs.actpool
}

// Consensus returns the consensus instance
func (cs *ChainService) Consensus() consensus.Consensus {
	return cs.consensus
}

// BlockSync returns the block syncer
func (cs *ChainService) BlockSync() blocksync.BlockSync {
	return cs.blocksync
}

// IndexService returns the indexservice instance
func (cs *ChainService) IndexService() *indexservice.Server {
	return cs.indexservice
}

// Explorer returns the explorer instance
func (cs *ChainService) Explorer() *explorer.Server {
	return cs.explorer
}

// RegisterProtocol register a protocol
func (cs *ChainService) RegisterProtocol(id string, p protocol.Protocol) error {
	if err := cs.registry.Register(id, p); err != nil {
		return err
	}
	cs.chain.GetFactory().AddActionHandlers(p)
	cs.actpool.AddActionValidators(p)
	cs.chain.Validator().AddActionValidators(p)
	return nil
}

// Registry returns a pointer to the registry
func (cs *ChainService) Registry() *protocol.Registry { return cs.registry }
