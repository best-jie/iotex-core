// Copyright (c) 2019 IoTeX
// This is an alpha (internal) release and is not suitable for production. This source code is provided 'as is' and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package rewarding

import (
	"context"
	"math/big"

	"go.uber.org/zap"

	"github.com/iotexproject/iotex-core/action"
	"github.com/iotexproject/iotex-core/action/protocol"
	"github.com/iotexproject/iotex-core/action/protocol/account/util"
	"github.com/iotexproject/iotex-core/address"
	"github.com/iotexproject/iotex-core/pkg/hash"
	"github.com/iotexproject/iotex-core/pkg/log"
)

const (
	// ProtocolID is the protocol ID
	// TODO: it works only for one instance per protocol definition now
	ProtocolID = "rewarding"
)

var (
	adminKey                    = []byte("admin")
	fundKey                     = []byte("fund")
	blockRewardHistoryKeyPrefix = []byte("blockRewardHistory")
	epochRewardHistoryKeyPrefix = []byte("epochRewardHistory")
	accountKeyPrefix            = []byte("account")
)

// Protocol defines the protocol of the rewarding fund and the rewarding process. It allows the admin to config the
// reward amount, users to donate tokens to the fund, block producers to grant them block and epoch reward and,
// beneficiaries to claim the balance into their personal account.
type Protocol struct {
	keyPrefix []byte
	addr      address.Address
}

// NewProtocol instantiates a rewarding protocol instance.
func NewProtocol() *Protocol {
	h := hash.Hash160b([]byte(ProtocolID))
	addr, err := address.FromBytes(h[:])
	if err != nil {
		log.L().Panic("Error when constructing the address of rewarding protocol", zap.Error(err))
	}
	return &Protocol{
		keyPrefix: h[:],
		addr:      addr,
	}
}

// Handle handles the actions on the rewarding protocol
func (p *Protocol) Handle(
	ctx context.Context,
	act action.Action,
	sm protocol.StateManager,
) (*action.Receipt, error) {
	// TODO: simplify the boilerplate
	switch act := act.(type) {
	case *action.SetReward:
		switch act.RewardType() {
		case action.BlockReward:
			if err := p.SetBlockReward(ctx, sm, act.Amount()); err != nil {
				return p.settleAction(ctx, sm, 1), nil
			}
			return p.settleAction(ctx, sm, 0), nil
		case action.EpochReward:
			if err := p.SetEpochReward(ctx, sm, act.Amount()); err != nil {
				return p.settleAction(ctx, sm, 1), nil
			}
			return p.settleAction(ctx, sm, 0), nil
		}
	case *action.DepositToRewardingFund:
		if err := p.Deposit(ctx, sm, act.Amount()); err != nil {
			return p.settleAction(ctx, sm, 1), nil
		}
		return p.settleAction(ctx, sm, 0), nil
	case *action.ClaimFromRewardingFund:
		if err := p.Claim(ctx, sm, act.Amount()); err != nil {
			return p.settleAction(ctx, sm, 1), nil
		}
		return p.settleAction(ctx, sm, 0), nil
	case *action.GrantReward:
		switch act.RewardType() {
		case action.BlockReward:
			if err := p.GrantBlockReward(ctx, sm); err != nil {
				return p.settleAction(ctx, sm, 1), nil
			}
			return p.settleAction(ctx, sm, 0), nil
		case action.EpochReward:
			if err := p.GrantEpochReward(ctx, sm); err != nil {
				return p.settleAction(ctx, sm, 1), nil
			}
			return p.settleAction(ctx, sm, 0), nil
		}
	}
	return nil, nil
}

// Validate validates the actions on the rewarding protocol
func (p *Protocol) Validate(
	ctx context.Context,
	act action.Action,
) error {
	// TODO: validate interface shouldn't be required for protocol code
	return nil
}

func (p *Protocol) state(sm protocol.StateManager, key []byte, value interface{}) error {
	keyHash := hash.Hash160b(append(p.keyPrefix, key...))
	return sm.State(keyHash, value)
}

func (p *Protocol) putState(sm protocol.StateManager, key []byte, value interface{}) error {
	keyHash := hash.Hash160b(append(p.keyPrefix, key...))
	return sm.PutState(keyHash, value)
}

func (p *Protocol) deleteState(sm protocol.StateManager, key []byte) error {
	keyHash := hash.Hash160b(append(p.keyPrefix, key...))
	return sm.DelState(keyHash)
}

func (p *Protocol) settleAction(
	ctx context.Context,
	sm protocol.StateManager,
	status uint64,
) *action.Receipt {
	raCtx := protocol.MustGetRunActionsCtx(ctx)
	gasFee := big.NewInt(0).Mul(raCtx.GasPrice, big.NewInt(0).SetUint64(raCtx.IntrinsicGas))
	if err := DepositGas(ctx, sm, gasFee, raCtx.Registry); err != nil {
		p.createReceipt(1, raCtx.ActionHash, raCtx.IntrinsicGas)
	}
	if err := p.increaseNonce(sm, raCtx.Caller, raCtx.Nonce); err != nil {
		return p.createReceipt(1, raCtx.ActionHash, raCtx.IntrinsicGas)
	}
	return p.createReceipt(status, raCtx.ActionHash, raCtx.IntrinsicGas)
}

func (p *Protocol) increaseNonce(sm protocol.StateManager, addr address.Address, nonce uint64) error {
	acc, err := util.LoadOrCreateAccount(sm, addr.String(), big.NewInt(0))
	if err != nil {
		return err
	}
	// TODO: this check shouldn't be necessary
	if nonce > acc.Nonce {
		acc.Nonce = nonce
	}
	if err := util.StoreAccount(sm, addr.String(), acc); err != nil {
		return err
	}
	return nil
}

func (p *Protocol) createReceipt(status uint64, actHash hash.Hash256, gasConsumed uint64) *action.Receipt {
	// TODO: need to review the fields
	return &action.Receipt{
		ReturnValue:     nil,
		Status:          0,
		ActHash:         actHash,
		GasConsumed:     gasConsumed,
		ContractAddress: p.addr.String(),
		Logs:            nil,
	}
}
