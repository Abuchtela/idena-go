package mempool

import (
	"crypto/ecdsa"
	"github.com/idena-network/idena-go/blockchain/attachments"
	"github.com/idena-network/idena-go/blockchain/types"
	"github.com/idena-network/idena-go/blockchain/validation"
	"github.com/idena-network/idena-go/common"
	"github.com/idena-network/idena-go/common/eventbus"
	"github.com/idena-network/idena-go/config"
	"github.com/idena-network/idena-go/core/appstate"
	"github.com/idena-network/idena-go/core/state"
	"github.com/idena-network/idena-go/crypto"
	"github.com/idena-network/idena-go/secstore"
	"github.com/idena-network/idena-go/stats/collector"
	"github.com/stretchr/testify/require"
	"github.com/tendermint/tm-db"
	"math/big"
	"testing"
	"time"
)

func TestTxPool_addDeferredTx(t *testing.T) {
	bus := eventbus.New()
	appState, _ := appstate.NewAppState(db.NewMemDB(), bus)

	key, _ := crypto.GenerateKey()
	secStore := secstore.NewSecStore()
	secStore.AddKey(crypto.FromECDSA(key))
	pool := NewTxPool(appState, bus, &config.Config{Mempool: &config.Mempool{TxPoolQueueSlots: -1, TxPoolAddrQueueLimit: -1}, Consensus: config.GetDefaultConsensusConfig()}, collector.NewStatsCollector())
	r := require.New(t)

	key, _ = crypto.GenerateKey()

	address := crypto.PubkeyToAddress(key.PublicKey)

	balance := new(big.Int).Mul(common.DnaBase, big.NewInt(100))

	appState.State.SetBalance(address, balance)
	appState.Commit(nil, true)
	appState.Initialize(0)
	pool.Initialize(
		&types.Header{
			EmptyBlockHeader: &types.EmptyBlockHeader{
				Height: 0,
			},
		}, common.Address{0x1}, false,
	)
	pool.StartSync()

	tx := &types.Transaction{
		AccountNonce: 1,
		To:           &address,
		Epoch:        0,
		Type:         types.SendTx,
		Amount:       new(big.Int).Mul(common.DnaBase, big.NewInt(1)),
	}

	tx, err := types.SignTx(tx, key)
	r.NoError(err)

	err = pool.AddInternalTx(tx)
	r.NoError(err)
	r.Len(pool.deferredTxs, 1)
	r.True(pool.knownDeferredTxs.Contains(tx.Hash()))

	pool.StopSync(&types.Block{
		Header: &types.Header{
			EmptyBlockHeader: &types.EmptyBlockHeader{
				Height: 1,
			},
		},
		Body: &types.Body{},
	})

	r.Len(pool.deferredTxs, 0)
	r.True(pool.knownDeferredTxs.Cardinality() == 0)
	r.Len(pool.executableTxs, 1)
}

func TestTxPool_ResetTo(t *testing.T) {
	pool := getPool()

	keys := make([]*ecdsa.PrivateKey, 0)

	for i := 0; i < 1200; i++ {
		key, _ := crypto.GenerateKey()
		address := crypto.PubkeyToAddress(key.PublicKey)
		keys = append(keys, key)
		pool.appState.State.SetBalance(address, big.NewInt(0).Mul(big.NewInt(10000), common.DnaBase))
	}

	pool.appState.Commit(nil, true)
	pool.appState.Initialize(1)
	pool.head = &types.Header{
		EmptyBlockHeader: &types.EmptyBlockHeader{
			Height: 1,
		},
	}
	getTx := func(key *ecdsa.PrivateKey) *types.Transaction {
		address := crypto.PubkeyToAddress(key.PublicKey)

		nonce := pool.appState.NonceCache.GetNonce(address, 0)

		tx := &types.Transaction{
			AccountNonce: nonce + 1,
			To:           &address,
			Epoch:        0,
			Type:         types.SendTx,
			Amount:       new(big.Int).Mul(common.DnaBase, big.NewInt(1)),
		}

		tx, _ = types.SignTx(tx, key)
		return tx
	}

	for i := 0; i < 255; i++ {
		for j := 0; j < 35; j++ {
			require.NoError(t, pool.AddInternalTx(getTx(keys[i])))
		}
	}
	for i := 255; i < 1024; i++ {
		for j := 0; j < 32; j++ {
			require.NoError(t, pool.AddInternalTx(getTx(keys[i])))
		}
	}
	for i := 1023; i < 1200; i++ {
		require.NoError(t, pool.AddInternalTx(getTx(keys[i])))
	}
	allTxsCount := len(pool.all.txs)

	require.Len(t, pool.executableTxs, 1200)
	require.Len(t, pool.pendingTxs, 256)

	full := true
	for _, e := range pool.executableTxs {
		full = full && (len(e.txs) == 32 || len(e.txs) == 1)
	}
	require.True(t, full)

	assertTxs := func() {
		cnt := 0
		for _, txs := range pool.executableTxs {
			cnt += len(txs.txs)
		}
		for _, txs := range pool.pendingTxs {
			cnt += len(txs.txs)
		}
		require.Equal(t, cnt, len(pool.all.txs))
	}
	assertTxs()
	for height := 2; height <= 13; height++ {

		builtTxs := pool.BuildBlockTransactions()
		require.True(t, len(builtTxs) > 0)
		for _, tx := range builtTxs {
			sender, _ := types.Sender(tx)
			pool.appState.State.SetNonce(sender, tx.AccountNonce)
		}
		pool.appState.Commit(nil, true)

		pool.ResetTo(&types.Block{
			Header: &types.Header{ProposedHeader: &types.ProposedHeader{
				Height: uint64(height),
			}},
			Body: &types.Body{
				Transactions: builtTxs,
			},
		})
		require.Equal(t, allTxsCount-len(builtTxs), len(pool.all.txs))
		allTxsCount = len(pool.all.txs)
		assertTxs()
	}

	require.Equal(t, 0, len(pool.all.txs))
	require.Equal(t, 0, len(pool.executableTxs))
	require.Equal(t, 0, len(pool.pendingTxs))
}

func getPool() *TxPool {
	bus := eventbus.New()
	appState, _ := appstate.NewAppState(db.NewMemDB(), bus)
	return NewTxPool(appState, bus, &config.Config{Mempool: config.GetDefaultMempoolConfig(), Consensus: config.GetDefaultConsensusConfig()}, collector.NewStatsCollector())
}

func TestSortedTxs_Remove(t *testing.T) {
	sortedTxs := newSortedTxs(10)
	require.NoError(t, sortedTxs.Add(&types.Transaction{
		AccountNonce: 2,
		Epoch:        1,
	}))
	require.NoError(t, sortedTxs.Add(&types.Transaction{
		AccountNonce: 3,
		Epoch:        1,
	}))

	require.Error(t, sortedTxs.Add(&types.Transaction{
		AccountNonce: 3,
		Epoch:        2,
	}))

	require.Error(t, sortedTxs.Add(&types.Transaction{
		AccountNonce: 5,
		Epoch:        1,
	}))

	require.NoError(t, sortedTxs.Add(&types.Transaction{
		AccountNonce: 4,
		Epoch:        1,
	}))

	sortedTxs.Remove(&types.Transaction{
		AccountNonce: 2,
		Epoch:        1,
	})

	require.Len(t, sortedTxs.txs, 2)

	require.Equal(t, uint32(3), sortedTxs.txs[0].AccountNonce)
	require.Equal(t, uint32(4), sortedTxs.txs[1].AccountNonce)

	sortedTxs.Remove(&types.Transaction{
		AccountNonce: 4,
		Epoch:        1,
	})

	require.Len(t, sortedTxs.txs, 1)
}

func TestTxMap_Sorted(t *testing.T) {
	txMap := newTxMap(4)
	require.NoError(t, txMap.Add(&types.Transaction{
		AccountNonce: 1,
		Epoch:        1,
	}))
	require.NoError(t, txMap.Add(&types.Transaction{
		AccountNonce: 2,
		Epoch:        1,
	}))
	require.NoError(t, txMap.Add(&types.Transaction{
		AccountNonce: 3,
		Epoch:        1,
	}))
	require.NoError(t, txMap.Add(&types.Transaction{
		AccountNonce: 1,
		Epoch:        2,
	}))

	require.Error(t, txMap.Add(&types.Transaction{
		AccountNonce: 5,
		Epoch:        1,
	}))

	sorted := txMap.Sorted()
	require.Len(t, sorted, 4)

	require.Equal(t, uint32(1), sorted[0].AccountNonce)
	require.Equal(t, uint16(1), sorted[0].Epoch)
	require.Equal(t, uint32(2), sorted[1].AccountNonce)
	require.Equal(t, uint32(3), sorted[2].AccountNonce)
	require.Equal(t, uint32(1), sorted[3].AccountNonce)

	require.NoError(t, txMap.Add(&types.Transaction{
		AccountNonce: 4,
		Epoch:        1,
		Type:         types.SubmitShortAnswersTx,
	}))

	sorted = txMap.Sorted()
	require.Equal(t, uint32(4), sorted[3].AccountNonce)
}

func TestTxPool_AddWithTxKeeper(t *testing.T) {

	txKeeperPersistInterval = time.Millisecond * 200

	pool := getPool()

	keys := make([]*ecdsa.PrivateKey, 0)

	for i := 0; i < 300; i++ {
		key, _ := crypto.GenerateKey()
		address := crypto.PubkeyToAddress(key.PublicKey)
		keys = append(keys, key)
		pool.appState.State.SetBalance(address, big.NewInt(0).Mul(big.NewInt(10000), common.DnaBase))
	}

	pool.appState.Commit(nil, true)
	pool.appState.Initialize(1)
	pool.Initialize(&types.Header{
		EmptyBlockHeader: &types.EmptyBlockHeader{
			Height: 1,
		},
	}, common.Address{0x1}, true)
	getTx := func(key *ecdsa.PrivateKey) *types.Transaction {
		address := crypto.PubkeyToAddress(key.PublicKey)

		nonce := pool.appState.NonceCache.GetNonce(address, 0)

		tx := &types.Transaction{
			AccountNonce: nonce + 1,
			To:           &address,
			Epoch:        0,
			Type:         types.SendTx,
			Amount:       new(big.Int).Mul(common.DnaBase, big.NewInt(1)),
		}

		tx, _ = types.SignTx(tx, key)
		return tx
	}

	for i := 0; i < 300; i++ {
		require.NoError(t, pool.AddInternalTx(getTx(keys[i])))
	}
	pool.txKeeper.persist()
	for i := 0; i < 20; i++ {
		require.NoError(t, pool.AddExternalTxs(validation.InboundTx, getTx(keys[i])))
	}
	time.Sleep(time.Second)
	require.Len(t, pool.txKeeper.txs, 320)

	pool.txKeeper.RemoveTxs([]common.Hash{pool.GetPendingTransaction(false, true, common.MultiShard, false)[0].Hash()})
	time.Sleep(time.Second)

	prevPool := pool

	prevPool.ResetTo(&types.Block{Header: &types.Header{
		EmptyBlockHeader: &types.EmptyBlockHeader{
			Height: 1,
		},
	}, Body: &types.Body{}})

	// wait for async mempool saving
	prevPool.txKeeper.mutex.RLock()
	prevPool.txKeeper.persist()
	prevPool.txKeeper.mutex.RUnlock()

	pool = getPool()
	pool.appState = prevPool.appState
	pool.Initialize(&types.Header{
		EmptyBlockHeader: &types.EmptyBlockHeader{
			Height: 1,
		},
	}, common.Address{0x1}, true)
	time.Sleep(time.Second)
	require.Len(t, pool.txKeeper.txs, 319)
	require.Len(t, pool.all.txs, 319)

	pool.appState.State.SetGlobalEpoch(1)
	pool.appState.Commit(nil, true)

	pool.ResetTo(&types.Block{Header: &types.Header{
		EmptyBlockHeader: &types.EmptyBlockHeader{
			Height: 2,
		},
	}, Body: &types.Body{}})

	time.Sleep(time.Second)

	pool.txKeeper.mutex.RLock()
	pool.txKeeper.persist()
	pool.txKeeper.mutex.RUnlock()

	require.Len(t, pool.txKeeper.txs, 0)
	require.Len(t, pool.all.txs, 0)

	txKeeper := NewTxKeeper(pool.cfg.DataDir)
	txKeeper.Load()
	require.Len(t, pool.txKeeper.txs, 0)
}

func TestTxPool_RecoverValidationTxs_OnAfterLongSession(t *testing.T) {

	txKeeperPersistInterval = time.Millisecond * 200

	pool := getPool()

	key, _ := crypto.GenerateKey()
	address := crypto.PubkeyToAddress(key.PublicKey)
	pool.appState.State.SetBalance(address, big.NewInt(0).Mul(big.NewInt(10000), common.DnaBase))
	pool.appState.State.SetState(address, state.Candidate)

	pool.appState.State.SetValidationPeriod(state.LongSessionPeriod)

	pool.appState.Commit(nil, true)
	pool.appState.Initialize(1)
	pool.Initialize(&types.Header{
		EmptyBlockHeader: &types.EmptyBlockHeader{
			Height: 1,
		},
	}, common.Address{0x1}, true)

	getTx := func(key *ecdsa.PrivateKey) *types.Transaction {

		address := crypto.PubkeyToAddress(key.PublicKey)

		nonce := pool.appState.NonceCache.GetNonce(address, 0)

		attachment := attachments.CreateShortAnswerAttachment([]byte{0x1}, 1, 1)

		tx := &types.Transaction{
			AccountNonce: nonce + 1,
			Epoch:        0,
			Type:         types.SubmitShortAnswersTx,
			Payload:      attachment,
		}

		tx, _ = types.SignTx(tx, key)
		return tx
	}

	require.NoError(t, pool.AddExternalTxs(validation.InboundTx, getTx(key)))

	time.Sleep(time.Millisecond * 500)
	prevPool := pool
	pool = getPool()
	pool.appState = prevPool.appState
	pool.appState.State.SetValidationPeriod(state.AfterLongSessionPeriod)

	pool.appState.Commit(nil, true)

	pool.Initialize(&types.Header{
		EmptyBlockHeader: &types.EmptyBlockHeader{
			Height: 2,
		},
	}, common.Address{0x1}, true)

	require.Len(t, pool.all.txs, 1)

	key2, _ := crypto.GenerateKey()
	address2 := crypto.PubkeyToAddress(key2.PublicKey)
	pool.appState.State.SetBalance(address2, big.NewInt(0).Mul(big.NewInt(10000), common.DnaBase))
	pool.appState.State.SetState(address2, state.Candidate)
	pool.appState.Commit(nil, true)

	pool = getPool()
	pool.appState = prevPool.appState

	pool.Initialize(&types.Header{
		EmptyBlockHeader: &types.EmptyBlockHeader{
			Height: 3,
		},
	}, common.Address{0x1}, true)

	require.Error(t, pool.AddInternalTx(getTx(key2)))
	require.Error(t, pool.AddExternalTxs(validation.InboundTx, getTx(key2)))
	require.Len(t, pool.all.txs, 1)
	require.NoError(t, pool.AddExternalTxs(validation.InBlockTx, getTx(key2)))
}
