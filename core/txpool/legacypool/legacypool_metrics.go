package legacypool

import (
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

type txpoolMetrics struct {
	lastReset int64
	Mu        struct {
		Add         muMetric
		RunReorg    muMetric
		Journal     muMetric
		Nonce       muMetric
		Report      muMetric
		Evict       muMetric
		Reannounce  muMetric
		SetGasTip   muMetric
		Stats       muMetric
		Content     muMetric
		ContentFrom muMetric
		Status      muMetric
	}
	Tps struct {
		Add           tpsMetric
		Queue2pending tpsMetric //promote
		Pending2P2P   tpsMetric //promote -> p2p
		Pending2Nil   tpsMetric //demote
	}
	KeyPoints struct {
		Add      keypointMetric
		RunReorg keypointMetric
	}
	Counters struct {
		unpromited counter // those transactions that are not able to be promoted (gapped)
	}
}

func (tm *txpoolMetrics) report(oldHead, newHead *types.Header) {
	var from, to int64 = 0, 0
	if oldHead != nil {
		from = int64(oldHead.Number.Uint64())
	}
	if newHead != nil {
		to = int64(newHead.Number.Uint64())
	}
	var duration time.Duration
	if tm.lastReset == 0 {
		duration = time.Second
	} else {
		duration = time.Since(time.Unix(atomic.LoadInt64(&tm.lastReset), 0))
	}
	addExec, addWait := tm.Mu.Add.mu()
	runReorgExec, runReorgWait := tm.Mu.RunReorg.mu()
	journalExec, _ := tm.Mu.Journal.mu()
	nonceExec, _ := tm.Mu.Nonce.mu()
	reportExec, _ := tm.Mu.Report.mu()
	evitExec, _ := tm.Mu.Evict.mu()
	reannounceExec, _ := tm.Mu.Reannounce.mu()
	setGasTipExec, _ := tm.Mu.SetGasTip.mu()
	statsExec, _ := tm.Mu.Stats.mu()
	contentExec, _ := tm.Mu.Content.mu()
	contentFromExec, _ := tm.Mu.ContentFrom.mu()
	statusExec, _ := tm.Mu.Status.mu()

	muTotal := addExec + runReorgExec + journalExec + nonceExec + reportExec + evitExec + reannounceExec + setGasTipExec + statsExec + contentExec + contentFromExec + statusExec
	unpromoted := tm.Counters.unpromited.val()
	log.Info("Transaction pool reorged, mu lock", "from", from, "to", to, "duration", duration, "unpromoted", unpromoted, "mu", muTotal,
		"add", addExec, "runReorg", runReorgExec,
		"journal", journalExec, "nonce", nonceExec,
		"report", reportExec, "evit", evitExec,
		"reannounce", reannounceExec, "setGasTip", setGasTipExec,
		"stats", statsExec, "content", contentExec,
		"contentFrom", contentFromExec, "status", statusExec)

	addRun := tm.KeyPoints.Add.val()
	runReorgRun := tm.KeyPoints.RunReorg.val()
	log.Info("Transaction pool reorged, mu lock of runReorg and Add: ", "from", from, "to", to, "duration", duration, "runReorg.wait", runReorgWait, "runReorg.run", runReorgRun, "add.wait", addWait, "add.run", addRun)

	addCount, addTps, addCost := tm.Tps.Add.tps(time.Now())
	queue2pendingCount, queue2pendingTps, queue2pendingCost := tm.Tps.Queue2pending.tps(time.Now())
	pending2p2pCount, pending2p2pTps, pending2p2pCost := tm.Tps.Pending2P2P.tps(time.Now())
	pending2nilCount, pending2nilTps, pending2nilCost := tm.Tps.Pending2Nil.tps(time.Now())
	log.Info("Transaction pool reorged, throughput of tps", "from", from, "to", to, "duration", duration, "Add.tps", addTps, "queue2pending.tps", queue2pendingTps, "pending2p2p.tps", pending2p2pTps, "pending2nil.tps", pending2nilTps)
	log.Info("Transaction pool reorged, throughput of tps", "from", from, "to", to, "duration", duration, "Add.count", addCount, "queue2pending.count", queue2pendingCount, "pending2p2p.count", pending2p2pCount, "pending2nil.count", pending2nilCount)
	log.Info("Transaction pool reorged, throughput of cost", "from", from, "to", to, "duration", duration, "Add.cost", addCost, "queue2pending.cost", queue2pendingCost, "pending2p2p.cost", pending2p2pCost, "pending2nil.cost", pending2nilCost)
}

type counter int64

func (c *counter) inc(num int64) {
	atomic.AddInt64((*int64)(c), num)
}

func (c *counter) val() int64 {
	return atomic.SwapInt64((*int64)(c), 0)
}

type keypointMetric struct {
	runTime int64
}

func (k *keypointMetric) markRun(cost time.Duration) {
	atomic.AddInt64(&k.runTime, int64(cost))
}

func (k *keypointMetric) val() time.Duration {
	return time.Duration(atomic.SwapInt64(&k.runTime, 0))
}

type muMetric struct {
	exec int64
	wait int64
}

func (m *muMetric) markExec(cost time.Duration) {
	atomic.AddInt64(&m.exec, int64(cost))
}

func (m *muMetric) markWait(cost time.Duration) {
	atomic.AddInt64(&m.wait, int64(cost))
}

func (m *muMetric) mu() (exec, wait time.Duration) {
	exec = time.Duration(atomic.SwapInt64(&m.exec, 0))
	wait = time.Duration(atomic.SwapInt64(&m.wait, 0))
	return
}

// throughput is a struct that holds the throughput metrics of the transaction pool.
// it is used at all key points to measure the throughput of the whole path where a transaction comes into the pool.
type tpsMetric struct {
	cost    int64 // cost in nanoseconds, need to be handled with atomic, so let's use int64
	counter int64
}

func (t *tpsMetric) mark(cost time.Duration, count int) {
	atomic.AddInt64(&t.cost, int64(cost))
	atomic.AddInt64(&t.counter, int64(count))
}

// avgAndReset returns the average nanoseconds of a transaction that it takes to go through the path.
// it's not accurate, but it's good enough to give a rough idea of the throughput.
// metrics data will be reset after this call.
func (t *tpsMetric) tps(now time.Time) (count int64, tps int64, cost time.Duration) {
	cost = time.Duration(atomic.SwapInt64(&t.cost, 0))
	count = atomic.SwapInt64(&t.counter, 0)
	if count == 0 {
		tps = 0
	} else {
		tps = int64(time.Second) * count / int64(cost)
	}
	return
}
