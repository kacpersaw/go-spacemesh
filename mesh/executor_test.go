package mesh_test

import (
	"context"
	"errors"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"

	"github.com/spacemeshos/go-spacemesh/common/types"
	vm "github.com/spacemeshos/go-spacemesh/genvm"
	"github.com/spacemeshos/go-spacemesh/log/logtest"
	"github.com/spacemeshos/go-spacemesh/mesh"
	"github.com/spacemeshos/go-spacemesh/mesh/mocks"
	"github.com/spacemeshos/go-spacemesh/sql"
	"github.com/spacemeshos/go-spacemesh/sql/layers"
)

type testExecutor struct {
	exec *mesh.Executor
	db   sql.Executor
	mcs  *mocks.MockconservativeState
	mvm  *mocks.MockvmState
}

func newTestExecutor(t *testing.T) *testExecutor {
	ctrl := gomock.NewController(t)
	te := &testExecutor{
		db:  sql.InMemory(),
		mvm: mocks.NewMockvmState(ctrl),
		mcs: mocks.NewMockconservativeState(ctrl),
	}
	te.exec = mesh.NewExecutor(te.db, te.mvm, te.mcs, logtest.New(t))
	return te
}

func makeResults(lid types.LayerID, txs ...types.Transaction) []types.TransactionWithResult {
	var results []types.TransactionWithResult
	for _, tx := range txs {
		results = append(results, types.TransactionWithResult{
			Transaction: tx,
			TransactionResult: types.TransactionResult{
				Layer: lid,
			},
		})
	}
	return results
}

func TestExecutor_Execute(t *testing.T) {
	te := newTestExecutor(t)
	lid := types.GetEffectiveGenesis()
	require.NoError(t, layers.SetApplied(te.db, lid, types.EmptyBlockID))

	t.Run("layer already applied", func(t *testing.T) {
		require.ErrorIs(t, te.exec.Execute(context.Background(), lid, nil), mesh.ErrLayerApplied)
	})

	t.Run("layer out of order", func(t *testing.T) {
		require.ErrorIs(t, te.exec.Execute(context.Background(), lid.Add(2), nil), mesh.ErrLayerNotInOrder)
	})

	lid = lid.Add(1)
	t.Run("txs missing", func(t *testing.T) {
		block := types.NewExistingBlock(types.RandomBlockID(), types.InnerBlock{
			LayerIndex: lid,
			TxIDs:      types.RandomTXSet(10),
		})
		require.ErrorIs(t, te.exec.Execute(context.Background(), block.LayerIndex, block), sql.ErrNotFound)
	})

	t.Run("empty layer", func(t *testing.T) {
		te.mvm.EXPECT().Apply(vm.ApplyContext{Layer: lid}, nil, nil)
		te.mcs.EXPECT().UpdateCache(gomock.Any(), lid, types.EmptyBlockID, nil, nil)
		te.mvm.EXPECT().GetStateRoot()
		require.NoError(t, te.exec.Execute(context.Background(), lid, nil))
		require.NoError(t, layers.SetApplied(te.db, lid, types.EmptyBlockID))
	})

	lid = lid.Add(1)
	block := types.NewExistingBlock(types.RandomBlockID(), types.InnerBlock{
		LayerIndex: lid,
	})
	t.Run("empty block", func(t *testing.T) {
		te.mvm.EXPECT().Apply(vm.ApplyContext{Layer: lid}, gomock.Any(), block.Rewards).DoAndReturn(
			func(_ vm.ApplyContext, got []types.Transaction, _ []types.AnyReward) ([]types.Transaction, []types.TransactionWithResult, error) {
				require.Empty(t, got)
				return nil, nil, nil
			})
		te.mcs.EXPECT().UpdateCache(gomock.Any(), lid, block.ID(), nil, nil)
		te.mvm.EXPECT().GetStateRoot()
		require.NoError(t, te.exec.Execute(context.Background(), block.LayerIndex, block))
		require.NoError(t, layers.SetApplied(te.db, lid, block.ID()))
	})

	lid = lid.Add(1)
	block = types.NewExistingBlock(types.BlockID{1}, types.InnerBlock{
		LayerIndex: lid,
		TxIDs:      mesh.CreateAndSaveTxs(t, te.db, 10),
	})
	errInconceivable := errors.New("inconceivable")
	t.Run("vm failure", func(t *testing.T) {
		te.mvm.EXPECT().Apply(vm.ApplyContext{Layer: block.LayerIndex}, gomock.Any(), block.Rewards).DoAndReturn(
			func(_ vm.ApplyContext, got []types.Transaction, _ []types.AnyReward) ([]types.Transaction, []types.TransactionWithResult, error) {
				tids := make([]types.TransactionID, 0, len(got))
				for _, tx := range got {
					tids = append(tids, tx.ID)
				}
				require.Equal(t, block.TxIDs, tids)
				return nil, nil, errInconceivable
			})
		require.ErrorIs(t, te.exec.Execute(context.Background(), block.LayerIndex, block), errInconceivable)
	})

	var executed []types.TransactionWithResult
	var ineffective []types.Transaction
	t.Run("conservative cache failure", func(t *testing.T) {
		te.mvm.EXPECT().Apply(vm.ApplyContext{Layer: block.LayerIndex}, gomock.Any(), block.Rewards).DoAndReturn(
			func(_ vm.ApplyContext, got []types.Transaction, _ []types.AnyReward) ([]types.Transaction, []types.TransactionWithResult, error) {
				tids := make([]types.TransactionID, 0, len(got))
				for _, tx := range got {
					tids = append(tids, tx.ID)
				}
				require.Equal(t, block.TxIDs, tids)
				// make first tx ineffective
				ineffective = got[:1]
				executed = makeResults(block.LayerIndex, got[1:]...)
				return ineffective, executed, nil
			})
		te.mcs.EXPECT().UpdateCache(gomock.Any(), block.LayerIndex, block.ID(), gomock.Any(), gomock.Any()).DoAndReturn(
			func(_ context.Context, _ types.LayerID, _ types.BlockID, gotE []types.TransactionWithResult, gotI []types.Transaction) error {
				require.Equal(t, executed, gotE)
				require.Equal(t, ineffective, gotI)
				return errInconceivable
			})
		require.ErrorIs(t, te.exec.Execute(context.Background(), block.LayerIndex, block), errInconceivable)
	})

	t.Run("applied block", func(t *testing.T) {
		te.mvm.EXPECT().Apply(vm.ApplyContext{Layer: block.LayerIndex}, gomock.Any(), block.Rewards).DoAndReturn(
			func(_ vm.ApplyContext, got []types.Transaction, _ []types.AnyReward) ([]types.Transaction, []types.TransactionWithResult, error) {
				tids := make([]types.TransactionID, 0, len(got))
				for _, tx := range got {
					tids = append(tids, tx.ID)
				}
				require.Equal(t, block.TxIDs, tids)
				// make first tx ineffective
				ineffective = got[:1]
				executed = makeResults(block.LayerIndex, got[1:]...)
				return ineffective, executed, nil
			})
		te.mcs.EXPECT().UpdateCache(gomock.Any(), block.LayerIndex, block.ID(), gomock.Any(), gomock.Any()).DoAndReturn(
			func(_ context.Context, _ types.LayerID, _ types.BlockID, gotE []types.TransactionWithResult, gotI []types.Transaction) error {
				require.Equal(t, executed, gotE)
				require.Equal(t, ineffective, gotI)
				return nil
			})
		te.mvm.EXPECT().GetStateRoot()
		require.NoError(t, te.exec.Execute(context.Background(), block.LayerIndex, block))
		require.NoError(t, layers.SetApplied(te.db, lid, block.ID()))
	})
}

func TestExecutor_ExecuteOptimistic(t *testing.T) {
	te := newTestExecutor(t)
	lid := types.GetEffectiveGenesis()
	tickHeight := uint64(111)
	rewards := []types.AnyReward{
		{
			Coinbase: types.Address{1},
		},
		{
			Coinbase: types.Address{2},
		},
	}
	tids := mesh.CreateAndSaveTxs(t, te.db, 10)
	require.NoError(t, layers.SetApplied(te.db, lid, types.EmptyBlockID))

	t.Run("layer already applied", func(t *testing.T) {
		block, err := te.exec.ExecuteOptimistic(context.Background(), lid, tickHeight, rewards, tids)
		require.ErrorIs(t, err, mesh.ErrLayerApplied)
		require.Nil(t, block)
	})

	t.Run("layer out of order", func(t *testing.T) {
		block, err := te.exec.ExecuteOptimistic(context.Background(), lid.Add(2), tickHeight, rewards, tids)
		require.ErrorIs(t, err, mesh.ErrLayerNotInOrder)
		require.Nil(t, block)
	})

	lid = lid.Add(1)
	t.Run("txs missing", func(t *testing.T) {
		block, err := te.exec.ExecuteOptimistic(context.Background(), lid, tickHeight, rewards, types.RandomTXSet(100))
		require.ErrorIs(t, err, sql.ErrNotFound)
		require.Nil(t, block)
	})

	errInconceivable := errors.New("inconceivable")
	t.Run("vm failure", func(t *testing.T) {
		te.mvm.EXPECT().Apply(vm.ApplyContext{Layer: lid}, gomock.Any(), rewards).DoAndReturn(
			func(_ vm.ApplyContext, got []types.Transaction, _ []types.AnyReward) ([]types.Transaction, []types.TransactionWithResult, error) {
				gotTids := make([]types.TransactionID, 0, len(got))
				for _, tx := range got {
					gotTids = append(gotTids, tx.ID)
				}
				require.Equal(t, tids, gotTids)
				return nil, nil, errInconceivable
			})
		block, err := te.exec.ExecuteOptimistic(context.Background(), lid, tickHeight, rewards, tids)
		require.ErrorIs(t, err, errInconceivable)
		require.Nil(t, block)
	})

	var executed []types.TransactionWithResult
	var ineffective []types.Transaction
	t.Run("conservative cache failure", func(t *testing.T) {
		te.mvm.EXPECT().Apply(vm.ApplyContext{Layer: lid}, gomock.Any(), rewards).DoAndReturn(
			func(_ vm.ApplyContext, got []types.Transaction, _ []types.AnyReward) ([]types.Transaction, []types.TransactionWithResult, error) {
				gotTids := make([]types.TransactionID, 0, len(got))
				for _, tx := range got {
					gotTids = append(gotTids, tx.ID)
				}
				require.Equal(t, tids, gotTids)
				// make first tx ineffective
				ineffective = got[:1]
				executed = makeResults(lid, got[1:]...)
				return ineffective, executed, nil
			})
		expBlock := &types.Block{
			InnerBlock: types.InnerBlock{
				LayerIndex: lid,
				TickHeight: tickHeight,
				Rewards:    rewards,
				TxIDs:      tids[1:],
			},
		}
		expBlock.Initialize()
		te.mcs.EXPECT().UpdateCache(gomock.Any(), lid, expBlock.ID(), gomock.Any(), gomock.Any()).DoAndReturn(
			func(_ context.Context, _ types.LayerID, _ types.BlockID, gotE []types.TransactionWithResult, gotI []types.Transaction) error {
				require.Equal(t, executed, gotE)
				require.Equal(t, ineffective, gotI)
				return errInconceivable
			})
		block, err := te.exec.ExecuteOptimistic(context.Background(), lid, tickHeight, rewards, tids)
		require.ErrorIs(t, err, errInconceivable)
		require.Nil(t, block)
	})

	t.Run("executed in situ", func(t *testing.T) {
		te.mvm.EXPECT().Apply(vm.ApplyContext{Layer: lid}, gomock.Any(), rewards).DoAndReturn(
			func(_ vm.ApplyContext, got []types.Transaction, _ []types.AnyReward) ([]types.Transaction, []types.TransactionWithResult, error) {
				gotTids := make([]types.TransactionID, 0, len(got))
				for _, tx := range got {
					gotTids = append(gotTids, tx.ID)
				}
				require.Equal(t, tids, gotTids)
				// make first tx ineffective
				ineffective = got[:1]
				executed = makeResults(lid, got[1:]...)
				return ineffective, executed, nil
			})
		expBlock := &types.Block{
			InnerBlock: types.InnerBlock{
				LayerIndex: lid,
				TickHeight: tickHeight,
				Rewards:    rewards,
				TxIDs:      tids[1:],
			},
		}
		expBlock.Initialize()
		te.mcs.EXPECT().UpdateCache(gomock.Any(), expBlock.LayerIndex, expBlock.ID(), gomock.Any(), gomock.Any()).DoAndReturn(
			func(_ context.Context, _ types.LayerID, _ types.BlockID, gotE []types.TransactionWithResult, gotI []types.Transaction) error {
				require.Equal(t, executed, gotE)
				require.Equal(t, ineffective, gotI)
				return nil
			})
		te.mvm.EXPECT().GetStateRoot()
		block, err := te.exec.ExecuteOptimistic(context.Background(), lid, tickHeight, rewards, tids)
		require.NoError(t, err)
		require.Equal(t, expBlock, block)
		require.NoError(t, layers.SetApplied(te.db, lid, block.ID()))
	})

	lid = lid.Add(1)
	t.Run("no txs in block", func(t *testing.T) {
		te.mvm.EXPECT().Apply(vm.ApplyContext{Layer: lid}, gomock.Any(), rewards).DoAndReturn(
			func(_ vm.ApplyContext, got []types.Transaction, _ []types.AnyReward) ([]types.Transaction, []types.TransactionWithResult, error) {
				require.Empty(t, got)
				return nil, nil, nil
			})
		expBlock := &types.Block{
			InnerBlock: types.InnerBlock{
				LayerIndex: lid,
				TickHeight: tickHeight,
				Rewards:    rewards,
			},
		}
		expBlock.Initialize()
		te.mcs.EXPECT().UpdateCache(gomock.Any(), lid, expBlock.ID(), nil, nil)
		te.mvm.EXPECT().GetStateRoot()
		block, err := te.exec.ExecuteOptimistic(context.Background(), lid, tickHeight, rewards, nil)
		require.NoError(t, err)
		require.Equal(t, expBlock, block)
		require.NoError(t, layers.SetApplied(te.db, lid, block.ID()))
	})
}

func TestExecutor_Revert(t *testing.T) {
	te := newTestExecutor(t)
	lid := types.GetEffectiveGenesis()
	require.NoError(t, layers.SetApplied(te.db, lid.Add(1), types.RandomBlockID()))

	errInconceivable := errors.New("inconceivable")
	t.Run("vm failure", func(t *testing.T) {
		te.mvm.EXPECT().Revert(lid).Return(errInconceivable)
		require.ErrorIs(t, te.exec.Revert(context.Background(), lid), errInconceivable)
	})

	t.Run("conservative state failure", func(t *testing.T) {
		te.mvm.EXPECT().Revert(lid)
		te.mcs.EXPECT().RevertCache(lid).Return(errInconceivable)
		require.ErrorIs(t, te.exec.Revert(context.Background(), lid), errInconceivable)
	})

	t.Run("revert success", func(t *testing.T) {
		te.mvm.EXPECT().Revert(lid)
		te.mcs.EXPECT().RevertCache(lid)
		te.mvm.EXPECT().GetStateRoot()
		require.NoError(t, te.exec.Revert(context.Background(), lid))
	})
}