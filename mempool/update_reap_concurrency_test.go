package mempool

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	abciclient "github.com/cometbft/cometbft/abci/client"
	abciclimocks "github.com/cometbft/cometbft/abci/client/mocks"
	abcitypes "github.com/cometbft/cometbft/abci/types"
	abci "github.com/cometbft/cometbft/api/cometbft/abci/v1"
	"github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/internal/test"
	"github.com/cometbft/cometbft/types"
)

// Set the CheckTx function to be instant, and the recheck function
// to be a recheckDelay sleep.
func mockClientWithInstantCheckDelayedRecheck(recheckDelay time.Duration) *abciclimocks.Client {
	var callback abciclient.Callback

	mockClient := new(abciclimocks.Client)
	mockClient.On("Start").Return(nil)
	mockClient.On("SetLogger", mock.Anything)
	mockClient.On("SetResponseCallback", mock.MatchedBy(func(cb abciclient.Callback) bool { callback = cb; return true }))
	mockClient.On("Error").Return(nil)
	mockClient.On("Flush", mock.Anything).Return(nil)

	mockClient.On("CheckTxAsync", mock.Anything, mock.Anything).Run(
		func(args mock.Arguments) {
			req := args.Get(1).(*abci.CheckTxRequest)
			if req.Type == abci.CHECK_TX_TYPE_RECHECK {
				time.Sleep(recheckDelay)
			}
		},
	).Return(func(_ context.Context, req *abci.CheckTxRequest) (*abciclient.ReqRes, error) {
		abciReq := abcitypes.ToCheckTxRequest(req)
		resp := &abci.CheckTxResponse{Code: abci.CodeTypeOK, GasWanted: 100, GasUsed: 99}
		ret := abciclient.NewReqRes(abciReq)
		ret.Response = abcitypes.ToCheckTxResponse(resp)
		callback(abciReq, ret.Response)
		return ret, nil
	})

	return mockClient
}

func ensureCleanReapUpdateSharedState(t *testing.T, mp *CListMempool) {
	// ensure Reap<>Update shared state metrics are wiped
	state := &mp.recheck.recheckReapSharedState
	state.mtx.Lock()
	defer state.mtx.Unlock()
	require.Equal(t, int64(0), state.successfullyUpdatedTxs, "successfully updated Txs should be 0")
	require.Equal(t, int64(0), state.bytesUpdated, "bytesUpdated should be 0")
	require.Equal(t, int64(0), state.gasUpdated, "gasUpdated should be 0")
	require.False(t, state.isReaping)
}

func setupConcurrentUpdateReapTest(t *testing.T, numTxs int, configUpdates func(*config.Config)) (*CListMempool, []types.Tx, func()) {
	t.Helper()
	mockClient := mockClientWithInstantCheckDelayedRecheck(100 * time.Microsecond)
	conf := test.ResetTestRoot("mempool_test")
	conf.Mempool.Recheck = true
	configUpdates(conf)
	mp, cleanup := newMempoolWithAppAndConfigMock(conf, mockClient)

	initTxs := checkTxs(t, mp, numTxs)
	require.Equal(t, numTxs, mp.Size(), "mempool size should be %d", numTxs)

	return mp, initTxs, cleanup
}

func asyncRunEmptyUpdateWithWg(t *testing.T, mp *CListMempool, doneUpdating *atomic.Bool, wg *sync.WaitGroup) {
	mp.Lock()
	wg.Add(1)
	go func() {
		txs := []types.Tx{}
		txResults := []*abci.ExecTxResult{}
		err := mp.Update(1, txs, txResults, PreCheckMaxBytes(100000), nil)
		require.NoError(t, err, "update should not error")
		mp.Unlock()
		doneUpdating.Store(true)
		wg.Done()
	}()
}

// Test calling clist mempool Update, and then reap concurrently,
// with various mempool sizes at the start of Update.
// Set the CheckTx function to just be a 100microsecond sleep.
func TestUpdateAndReapConcurrently(t *testing.T) {
	confUpdate := func(conf *config.Config) {
		conf.Mempool.RecheckTimeout = time.Minute
	}
	mp, initTxs, cleanup := setupConcurrentUpdateReapTest(t, 500, confUpdate)
	defer cleanup()

	doneUpdating, wg := &atomic.Bool{}, &sync.WaitGroup{}
	asyncRunEmptyUpdateWithWg(t, mp, doneUpdating, wg)
	// give some time for update to start
	time.Sleep(200 * time.Microsecond)

	reapTxs := mp.ReapMaxTxs(100)
	require.Equal(t, 101, len(reapTxs), "reaped 101 txs")
	for i := 0; i < 101; i++ {
		require.Equal(t, initTxs[i], reapTxs[i], "reaped txs should be the same")
	}
	updateStatus := doneUpdating.Load()
	require.False(t, updateStatus, "reap waited until update was done")

	// ensure non-default Reap<>Update shared state metrics
	wg.Wait()
	// ensure Reap<>Update shared state metrics are wiped
	ensureCleanReapUpdateSharedState(t, mp)
}

func TestMultipleConcurrentReapsWhileUpdating(t *testing.T) {
	mp, _, cleanup := setupConcurrentUpdateReapTest(t, 500, func(conf *config.Config) {})
	defer cleanup()

	doneUpdating, wg := &atomic.Bool{}, &sync.WaitGroup{}
	asyncRunEmptyUpdateWithWg(t, mp, doneUpdating, wg)
	// give some time for update to start
	time.Sleep(200 * time.Microsecond)

	// Start multiple goroutines to perform reaps concurrently
	numReaps := 10
	go func() {
		mp.ReapMaxTxs(400)
	}()
	// give some time for the first reap to start
	time.Sleep(200 * time.Microsecond)

	// The first reap should be blocked for 50ms, plenty of time for
	// attempting 10 concurrent reaps that should all fail.
	for i := 0; i < numReaps; i++ {
		wg.Add(1)
		go func() {
			require.Panics(t, func() { mp.ReapMaxTxs(400) }, "concurrent reap should panic")
			wg.Done()
		}()
	}

	wg.Wait()
}