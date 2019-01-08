package blocks

import (
	"bytes"
	"errors"
	"math"

	"github.com/elastos/Elastos.ELA/blockchain"
	"github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/common/config"
	"github.com/elastos/Elastos.ELA/core/contract/program"
	"github.com/elastos/Elastos.ELA/core/types"
	"github.com/elastos/Elastos.ELA/core/types/outputpayload"
	"github.com/elastos/Elastos.ELA/version/verconf"
)

// Ensure blockCurrent implement the BlockVersion interface.
var _ BlockVersion = (*blockCurrent)(nil)

// blockCurrent represent the current block version.
type blockCurrent struct {
	cfg *verconf.Config
}

func (b *blockCurrent) GetVersion() uint32 {
	return 1
}

func (b *blockCurrent) GetNextOnDutyArbitrator(dutyChangedCount, offset uint32) []byte {
	arbitrators := b.cfg.Arbitrators.GetArbitrators()
	if len(arbitrators) == 0 {
		return nil
	}
	index := (dutyChangedCount + offset) % uint32(len(arbitrators))
	arbitrator := arbitrators[index]

	return arbitrator
}

func (b *blockCurrent) CheckConfirmedBlockOnFork(block *types.Block) error {
	if !b.cfg.Server.IsCurrent() {
		return nil
	}

	hash, err := b.cfg.ChainStore.GetBlockHash(block.Height)
	if err != nil {
		return err
	}

	anotherBlock, err := b.cfg.ChainStore.GetBlock(hash)
	if err != nil {
		return err
	}

	if block.Hash().IsEqual(anotherBlock.Hash()) {
		return nil
	}

	evidence, err := b.generateBlockEvidence(block)
	if err != nil {
		return err
	}

	compareEvidence, err := b.generateBlockEvidence(anotherBlock)
	if err != nil {
		return err
	}

	illegalBlocks := &types.PayloadIllegalBlock{
		DposIllegalBlocks: types.DposIllegalBlocks{
			CoinType:        types.ELACoin,
			BlockHeight:     block.Height,
			Evidence:        *evidence,
			CompareEvidence: *compareEvidence,
		},
	}

	if err := blockchain.CheckDposIllegalBlocks(&illegalBlocks.DposIllegalBlocks); err != nil {
		return err
	}

	tx := &types.Transaction{
		Version:        types.TransactionVersion(b.cfg.Versions.GetDefaultTxVersion(block.Height)),
		TxType:         types.IllegalBlockEvidence,
		PayloadVersion: types.PayloadIllegalBlockVersion,
		Payload:        illegalBlocks,
		Attributes:     []*types.Attribute{},
		LockTime:       0,
		Programs:       []*program.Program{},
		Outputs:        []*types.Output{},
		Inputs:         []*types.Input{},
		Fee:            0,
	}
	if err := b.cfg.TxMemPool.AppendToTxPool(tx); err == nil {
		err = b.cfg.TxMemPool.AppendToTxPool(tx)
	}

	return nil
}

func (b *blockCurrent) generateBlockEvidence(block *types.Block) (*types.BlockEvidence, error) {
	headerBuf := new(bytes.Buffer)
	if err := block.Header.Serialize(headerBuf); err != nil {
		return nil, err
	}

	confirm, err := b.cfg.ChainStore.GetConfirm(block.Hash())
	if err != nil {
		return nil, err
	}
	confirmBuf := new(bytes.Buffer)
	if err = confirm.Serialize(confirmBuf); err != nil {
		return nil, err
	}
	confirmSigners, err := b.getConfirmSigners(confirm)
	if err != nil {
		return nil, err
	}

	return &types.BlockEvidence{
		Block:        headerBuf.Bytes(),
		BlockConfirm: confirmBuf.Bytes(),
		Signers:      confirmSigners,
	}, nil
}

func (b *blockCurrent) getConfirmSigners(confirm *types.DPosProposalVoteSlot) ([][]byte, error) {
	result := make([][]byte, 0)
	for _, v := range confirm.Votes {
		data, err := common.HexStringToBytes(v.Signer)
		if err != nil {
			return nil, err
		}
		result = append(result, data)
	}
	return result, nil
}

func (b *blockCurrent) GetProducersDesc() ([][]byte, error) {
	producersInfo := b.cfg.ChainStore.GetRegisteredProducers()
	if uint32(len(producersInfo)) < config.Parameters.ArbiterConfiguration.NormalArbitratorsCount {
		return nil, errors.New("producers count less than min arbitrators count")
	}

	result := make([][]byte, 0)
	for i := uint32(0); i < uint32(len(producersInfo)); i++ {
		result = append(result, producersInfo[i].PublicKey)
	}
	return result, nil
}

func (b *blockCurrent) AddDposBlock(dposBlock *types.DposBlock) (bool, bool, error) {
	return b.cfg.BlockMemPool.AppendDposBlock(dposBlock)
}

func (b *blockCurrent) AssignCoinbaseTxRewards(block *types.Block, totalReward common.Fixed64) error {
	rewardCyberRepublic := common.Fixed64(math.Ceil(float64(totalReward) * 0.3))
	rewardDposArbiter := common.Fixed64(float64(totalReward) * 0.35)

	var dposChange common.Fixed64
	var err error
	if dposChange, err = b.distributeDposReward(block.Transactions[0], rewardDposArbiter); err != nil {
		return err
	}
	rewardMergeMiner := common.Fixed64(totalReward) - rewardCyberRepublic - rewardDposArbiter + dposChange
	block.Transactions[0].Outputs[0].Value = rewardCyberRepublic
	block.Transactions[0].Outputs[1].Value = rewardMergeMiner
	return nil
}

func (b *blockCurrent) distributeDposReward(coinBaseTx *types.Transaction, reward common.Fixed64) (common.Fixed64, error) {
	arbitratorsHashes := b.cfg.Arbitrators.GetArbitratorsProgramHashes()
	if uint32(len(arbitratorsHashes)) < blockchain.DefaultLedger.Arbitrators.GetArbitersCount() {
		return 0, errors.New("current arbitrators count less than required arbitrators count")
	}
	candidatesHashes := b.cfg.Arbitrators.GetCandidatesProgramHashes()

	totalBlockConfirmReward := float64(reward) * 0.25
	totalTopProducersReward := float64(reward) * 0.75
	individualBlockConfirmReward := common.Fixed64(math.Floor(totalBlockConfirmReward / float64(len(arbitratorsHashes))))
	individualProducerReward := common.Fixed64(math.Floor(totalTopProducersReward / float64(len(arbitratorsHashes)+len(candidatesHashes))))

	realDposReward := common.Fixed64(0)
	for _, v := range arbitratorsHashes {

		coinBaseTx.Outputs = append(coinBaseTx.Outputs, &types.Output{
			AssetID:       config.ELAAssetID,
			Value:         individualBlockConfirmReward + individualProducerReward,
			ProgramHash:   *v,
			OutputType:    types.DefaultOutput,
			OutputPayload: &outputpayload.DefaultOutput{},
		})

		realDposReward += individualBlockConfirmReward + individualProducerReward
	}

	for _, v := range candidatesHashes {

		coinBaseTx.Outputs = append(coinBaseTx.Outputs, &types.Output{
			AssetID:       config.ELAAssetID,
			Value:         individualProducerReward,
			ProgramHash:   *v,
			OutputType:    types.DefaultOutput,
			OutputPayload: &outputpayload.DefaultOutput{},
		})

		realDposReward += individualProducerReward
	}

	change := reward - realDposReward
	if change < 0 {
		return 0, errors.New("Real dpos reward more than reward limit.")
	}
	return change, nil
}

func NewBlockCurrent(cfg *verconf.Config) *blockCurrent {
	return &blockCurrent{cfg: cfg}
}
