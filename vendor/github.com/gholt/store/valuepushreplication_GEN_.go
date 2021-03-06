package store

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gholt/brimtime"
	"github.com/uber-go/zap"
)

type valuePushReplicationState struct {
	interval   int
	workers    int
	msgTimeout time.Duration

	startupShutdownLock sync.Mutex
	notifyChan          chan *bgNotification
	lists               [][]uint64
	valBufs             [][]byte
}

func (store *defaultValueStore) pushReplicationConfig(cfg *ValueStoreConfig) {
	store.pushReplicationState.interval = cfg.PushReplicationInterval
	store.pushReplicationState.workers = cfg.PushReplicationWorkers
	store.pushReplicationState.msgTimeout = time.Duration(cfg.PushReplicationMsgTimeout) * time.Millisecond
}

func (store *defaultValueStore) pushReplicationStartup() {
	store.pushReplicationState.startupShutdownLock.Lock()
	if store.pushReplicationState.notifyChan == nil {
		store.pushReplicationState.notifyChan = make(chan *bgNotification, 1)
		store.pushReplicationState.lists = nil
		store.pushReplicationState.valBufs = nil
		go store.pushReplicationLauncher(store.pushReplicationState.notifyChan)
	}
	store.pushReplicationState.startupShutdownLock.Unlock()
}

func (store *defaultValueStore) pushReplicationShutdown() {
	store.pushReplicationState.startupShutdownLock.Lock()
	if store.pushReplicationState.notifyChan != nil {
		c := make(chan struct{}, 1)
		store.pushReplicationState.notifyChan <- &bgNotification{
			action:   _BG_DISABLE,
			doneChan: c,
		}
		<-c
		store.pushReplicationState.notifyChan = nil
		store.pushReplicationState.lists = nil
		store.pushReplicationState.valBufs = nil
	}
	store.pushReplicationState.startupShutdownLock.Unlock()
}

func (store *defaultValueStore) pushReplicationLauncher(notifyChan chan *bgNotification) {
	interval := float64(store.pushReplicationState.interval) * float64(time.Second)
	store.randMutex.Lock()
	nextRun := time.Now().Add(time.Duration(interval + interval*store.rand.NormFloat64()*0.1))
	store.randMutex.Unlock()
	var notification *bgNotification
	running := true
	for running {
		if notification == nil {
			sleep := nextRun.Sub(time.Now())
			if sleep > 0 {
				select {
				case notification = <-notifyChan:
				case <-time.After(sleep):
				}
			} else {
				select {
				case notification = <-notifyChan:
				default:
				}
			}
		}
		store.randMutex.Lock()
		nextRun = time.Now().Add(time.Duration(interval + interval*store.rand.NormFloat64()*0.1))
		store.randMutex.Unlock()
		if notification != nil {
			var nextNotification *bgNotification
			switch notification.action {
			case _BG_PASS:
				nextNotification = store.pushReplicationPass(notifyChan)
			case _BG_DISABLE:
				running = false
			default:
				store.logger.Error("invalid action requested", zap.String("name", store.loggerPrefix+"pushReplication"), zap.Int("action", int(notification.action)))
			}
			notification.doneChan <- struct{}{}
			notification = nextNotification
		} else {
			notification = store.pushReplicationPass(notifyChan)
		}
	}
}

func (store *defaultValueStore) pushReplicationPass(notifyChan chan *bgNotification) *bgNotification {
	if store.msgRing == nil {
		return nil
	}
	begin := time.Now()
	defer func() {
		elapsed := time.Now().Sub(begin)
		store.logger.Debug("pass completed", zap.String("name", store.loggerPrefix+"pushReplication"), zap.Duration("elapsed", elapsed))
		atomic.StoreInt64(&store.outPushReplicationNanoseconds, elapsed.Nanoseconds())
	}()
	ring := store.msgRing.Ring()
	if ring == nil {
		return nil
	}
	ringVersion := ring.Version()
	pbc := ring.PartitionBitCount()
	partitionShift := uint64(64 - pbc)
	partitionMax := (uint64(1) << pbc) - 1
	workerMax := uint64(store.pushReplicationState.workers - 1)
	workerPartitionPiece := (uint64(1) << partitionShift) / (workerMax + 1)
	// To avoid memory churn, the scratchpad areas are allocated just once and
	// passed in to the workers.
	for len(store.pushReplicationState.lists) < int(workerMax+1) {
		store.pushReplicationState.lists = append(store.pushReplicationState.lists, make([]uint64, store.bulkSetState.msgCap/_VALUE_BULK_SET_MSG_MIN_ENTRY_LENGTH*2))
	}
	for len(store.pushReplicationState.valBufs) < int(workerMax+1) {
		store.pushReplicationState.valBufs = append(store.pushReplicationState.valBufs, make([]byte, store.valueCap))
	}
	var abort uint32
	work := func(partition uint64, worker uint64, list []uint64, valbuf []byte) {
		partitionOnLeftBits := partition << partitionShift
		rangeBegin := partitionOnLeftBits + (workerPartitionPiece * worker)
		var rangeEnd uint64
		// A little bit of complexity here to handle where the more general
		// expressions would have overflow issues.
		if worker != workerMax {
			rangeEnd = partitionOnLeftBits + (workerPartitionPiece * (worker + 1)) - 1
		} else {
			if partition != partitionMax {
				rangeEnd = ((partition + 1) << partitionShift) - 1
			} else {
				rangeEnd = math.MaxUint64
			}
		}
		timestampbitsNow := uint64(brimtime.TimeToUnixMicro(time.Now())) << _TSB_UTIL_BITS
		cutoff := timestampbitsNow - store.replicationIgnoreRecent
		tombstoneCutoff := timestampbitsNow - store.tombstoneDiscardState.age
		availableBytes := int64(store.bulkSetState.msgCap)
		list = list[:0]
		// We ignore the "more" option from ScanCallback and just send the
		// first matching batch each full iteration. Once a remote end acks the
		// batch, those keys will have been removed and the first matching
		// batch will start with any remaining keys.
		// First we gather the matching keys to send.
		store.locmap.ScanCallback(rangeBegin, rangeEnd, 0, _TSB_LOCAL_REMOVAL, cutoff, math.MaxUint64, func(keyA uint64, keyB uint64, timestampbits uint64, length uint32) bool {
			inMsgLength := _VALUE_BULK_SET_MSG_ENTRY_HEADER_LENGTH + int64(length)
			if timestampbits&_TSB_DELETION == 0 || timestampbits >= tombstoneCutoff {
				list = append(list, keyA, keyB)
				availableBytes -= inMsgLength
				if availableBytes < inMsgLength {
					return false
				}
			}
			return true
		})
		if len(list) <= 0 || atomic.LoadUint32(&abort) != 0 {
			return
		}
		ring2 := store.msgRing.Ring()
		if ring2 == nil || ring2.Version() != ringVersion {
			return
		}
		// Then we build and send the actual message.
		bsm := store.newOutBulkSetMsg()
		var timestampbits uint64
		var err error
		for i := 0; i < len(list); i += 2 {
			timestampbits, valbuf, err = store.read(list[i], list[i+1], valbuf[:0])
			// This might mean we need to send a deletion or it might mean the
			// key has been completely removed from our records
			// (timestampbits==0).
			if IsNotFound(err) {
				if timestampbits == 0 {
					continue
				}
			} else if err != nil {
				continue
			}
			if timestampbits&_TSB_LOCAL_REMOVAL == 0 && timestampbits < cutoff && (timestampbits&_TSB_DELETION == 0 || timestampbits >= tombstoneCutoff) {
				if !bsm.add(list[i], list[i+1], timestampbits, valbuf) {
					break
				}
				atomic.AddInt32(&store.outBulkSetPushValues, 1)
			}
		}
		atomic.AddInt32(&store.outBulkSetPushes, 1)
		store.msgRing.MsgToOtherReplicas(bsm, uint32(partition), store.pushReplicationState.msgTimeout)
	}
	wg := &sync.WaitGroup{}
	wg.Add(int(workerMax + 1))
	for worker := uint64(0); worker <= workerMax; worker++ {
		go func(worker uint64) {
			list := store.pushReplicationState.lists[worker]
			valbuf := store.pushReplicationState.valBufs[worker]
			partitionBegin := (partitionMax + 1) / (workerMax + 1) * worker
			for partition := partitionBegin; ; {
				if atomic.LoadUint32(&abort) != 0 {
					break
				}
				ring2 := store.msgRing.Ring()
				if ring2 == nil || ring2.Version() != ringVersion {
					break
				}
				if !ring.Responsible(uint32(partition)) {
					work(partition, worker, list, valbuf)
				}
				partition++
				if partition > partitionMax {
					partition = 0
				}
				if partition == partitionBegin {
					break
				}
			}
			wg.Done()
		}(worker)
	}
	waitChan := make(chan struct{}, 1)
	go func() {
		wg.Wait()
		close(waitChan)
	}()
	select {
	case notification := <-notifyChan:
		atomic.AddUint32(&abort, 1)
		<-waitChan
		return notification
	case <-waitChan:
		return nil
	}
}
