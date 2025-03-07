package updater

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"time"

	"github.com/jmhodges/clock"
	"github.com/prometheus/client_golang/prometheus"

	capb "github.com/letsencrypt/boulder/ca/proto"
	"github.com/letsencrypt/boulder/crl"
	cspb "github.com/letsencrypt/boulder/crl/storer/proto"
	"github.com/letsencrypt/boulder/issuance"
	blog "github.com/letsencrypt/boulder/log"
	sapb "github.com/letsencrypt/boulder/sa/proto"
)

type crlUpdater struct {
	issuers           map[issuance.IssuerNameID]*issuance.Certificate
	numShards         int
	lookbackPeriod    time.Duration
	lookforwardPeriod time.Duration
	updatePeriod      time.Duration
	updateOffset      time.Duration
	maxParallelism    int

	sa sapb.StorageAuthorityClient
	ca capb.CRLGeneratorClient
	cs cspb.CRLStorerClient

	tickHistogram       *prometheus.HistogramVec
	updatedCounter      *prometheus.CounterVec
	secondsSinceSuccess *prometheus.GaugeVec

	log blog.Logger
	clk clock.Clock
}

func NewUpdater(
	issuers []*issuance.Certificate,
	numShards int,
	certLifetime time.Duration,
	updatePeriod time.Duration,
	updateOffset time.Duration,
	maxParallelism int,
	sa sapb.StorageAuthorityClient,
	ca capb.CRLGeneratorClient,
	cs cspb.CRLStorerClient,
	stats prometheus.Registerer,
	log blog.Logger,
	clk clock.Clock,
) (*crlUpdater, error) {
	issuersByNameID := make(map[issuance.IssuerNameID]*issuance.Certificate, len(issuers))
	for _, issuer := range issuers {
		issuersByNameID[issuer.NameID()] = issuer
	}

	if numShards < 1 {
		return nil, fmt.Errorf("must have positive number of shards, got: %d", numShards)
	}

	if updatePeriod >= 7*24*time.Hour {
		return nil, fmt.Errorf("must update CRLs at least every 7 days, got: %s", updatePeriod)
	}

	if updateOffset >= updatePeriod {
		return nil, fmt.Errorf("update offset must be less than period: %s !< %s", updateOffset, updatePeriod)
	}

	// Set the lookback period to be significantly greater than the update period.
	// This guarantees that a certificate which was revoked very shortly before it
	// expired will still appear on at least one CRL, as required by RFC 5280
	// Section 3.3.
	lookbackPeriod := 4 * updatePeriod

	// Set the lookforward period to be greater than the lifetime of the longest
	// currently-valid certificate. Ensure it overshoots by more than the width
	// of one shard. See comment on getShardBoundaries for details.
	tentativeShardWidth := (lookbackPeriod + certLifetime).Nanoseconds() / int64(numShards)
	lookforwardPeriod := certLifetime + time.Duration(4*tentativeShardWidth)

	// Ensure that the total window (lookback + lookforward) is evenly divisible
	// by the number of shards, to make shard boundary calculations easy.
	window := lookbackPeriod + lookforwardPeriod
	offset := window.Nanoseconds() % int64(numShards)
	if offset != 0 {
		lookforwardPeriod += time.Duration(int64(numShards) - offset)
	}

	if maxParallelism <= 0 {
		maxParallelism = 1
	}

	tickHistogram := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "crl_updater_ticks",
		Help:    "A histogram of crl-updater tick latencies labeled by issuer and result",
		Buckets: []float64{0.01, 0.2, 0.5, 1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000},
	}, []string{"issuer", "result"})
	stats.MustRegister(tickHistogram)

	updatedCounter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "crl_updater_generated",
		Help: "A counter of CRL generation calls labeled by result",
	}, []string{"result"})
	stats.MustRegister(updatedCounter)

	secondsSinceSuccess := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "crl_updater_secs_since_success",
		Help: "The number of seconds since crl-updater last succeeded labeled by issuer",
	}, []string{"issuer"})
	stats.MustRegister(secondsSinceSuccess)

	return &crlUpdater{
		issuersByNameID,
		numShards,
		lookbackPeriod,
		lookforwardPeriod,
		updatePeriod,
		updateOffset,
		maxParallelism,
		sa,
		ca,
		cs,
		tickHistogram,
		updatedCounter,
		secondsSinceSuccess,
		log,
		clk,
	}, nil
}

// Run causes the crl-updater to run immediately, and then re-run continuously
// on the frequency specified by crlUpdater.updatePeriod. The provided context
// can be used to gracefully stop (cancel) the process.
func (cu *crlUpdater) Run(ctx context.Context) {
	// We don't want the times at which crl-updater runs to be dependent on when
	// the process starts. So wait until the appropriate time before kicking off
	// the first run and the main ticker loop.
	currOffset := cu.clk.Now().UnixNano() % cu.updatePeriod.Nanoseconds()
	var waitNanos int64
	if currOffset <= cu.updateOffset.Nanoseconds() {
		waitNanos = cu.updateOffset.Nanoseconds() - currOffset
	} else {
		waitNanos = cu.updatePeriod.Nanoseconds() - currOffset + cu.updateOffset.Nanoseconds()
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Duration(waitNanos)):
	}

	// Tick once immediately, but create the ticker first so that it starts
	// counting from the appropriate time.
	ticker := time.NewTicker(cu.updatePeriod)
	cu.Tick(ctx)

	for {
		// If we have overrun *and* been canceled, both of the below cases could be
		// selectable at the same time, so check for context cancellation first.
		if ctx.Err() != nil {
			ticker.Stop()
			return
		}
		select {
		case <-ticker.C:
			cu.Tick(ctx)
		case <-ctx.Done():
			ticker.Stop()
			return
		}
	}
}

// TODO(#6261): Unify all error messages to identify the shard they're working
// on as a JSON object including issuer, crl number, and shard number.

func (cu *crlUpdater) Tick(ctx context.Context) {
	atTime := cu.clk.Now()
	result := "success"
	defer func() {
		cu.tickHistogram.WithLabelValues("all", result).Observe(cu.clk.Since(atTime).Seconds())
	}()
	cu.log.Debugf("Ticking at time %s", atTime)

	for id, iss := range cu.issuers {
		// For now, process each issuer serially. This keeps the worker pool system
		// simple, and processing all of the issuers in parallel likely wouldn't
		// meaningfully speed up the overall process.
		err := cu.tickIssuer(ctx, atTime, id)
		if err != nil {
			cu.log.AuditErrf(
				"tick for issuer %s at time %s failed: %s",
				iss.Subject.CommonName,
				atTime.Format(time.RFC3339Nano),
				err)
			result = "failed"
		}
	}
}

// tickIssuer performs the full CRL issuance cycle for a single issuer cert.
func (cu *crlUpdater) tickIssuer(ctx context.Context, atTime time.Time, issuerNameID issuance.IssuerNameID) error {
	start := cu.clk.Now()
	result := "success"
	defer func() {
		cu.tickHistogram.WithLabelValues(cu.issuers[issuerNameID].Subject.CommonName+" (Overall)", result).Observe(cu.clk.Since(start).Seconds())
	}()
	cu.log.Debugf("Ticking issuer %d at time %s", issuerNameID, atTime)

	type shardResult struct {
		shardIdx int
		err      error
	}

	shardWorker := func(in <-chan int, out chan<- shardResult) {
		for idx := range in {
			select {
			case <-ctx.Done():
				return
			default:
				out <- shardResult{
					shardIdx: idx,
					err:      cu.tickShard(ctx, atTime, issuerNameID, idx),
				}
			}
		}
	}

	shardIdxs := make(chan int, cu.numShards)
	shardResults := make(chan shardResult, cu.numShards)
	for i := 0; i < cu.maxParallelism; i++ {
		go shardWorker(shardIdxs, shardResults)
	}

	for shardIdx := 0; shardIdx < cu.numShards; shardIdx++ {
		shardIdxs <- shardIdx
	}
	close(shardIdxs)

	for i := 0; i < cu.numShards; i++ {
		res := <-shardResults
		if res.err != nil {
			result = "failed"
			return fmt.Errorf("updating shard %d: %w", res.shardIdx, res.err)
		}
	}

	return nil
}

func (cu *crlUpdater) tickShard(ctx context.Context, atTime time.Time, issuerNameID issuance.IssuerNameID, shardIdx int) error {
	start := cu.clk.Now()
	crlId, err := crl.Id(issuerNameID, crl.Number(atTime), shardIdx)
	if err != nil {
		return err
	}
	result := "success"
	defer func() {
		cu.tickHistogram.WithLabelValues(cu.issuers[issuerNameID].Subject.CommonName, result).Observe(cu.clk.Since(start).Seconds())
		cu.updatedCounter.WithLabelValues(result).Inc()
	}()
	cu.log.Debugf("Ticking shard %d of issuer %d at time %s", shardIdx, issuerNameID, atTime)

	expiresAfter, expiresBefore := cu.getShardBoundaries(atTime, shardIdx)

	saStream, err := cu.sa.GetRevokedCerts(ctx, &sapb.GetRevokedCertsRequest{
		IssuerNameID:  int64(issuerNameID),
		ExpiresAfter:  expiresAfter.UnixNano(),
		ExpiresBefore: expiresBefore.UnixNano(),
		RevokedBefore: atTime.UnixNano(),
	})
	if err != nil {
		result = "failed"
		return fmt.Errorf("connecting to SA for %s: %w", crlId, err)
	}

	caStream, err := cu.ca.GenerateCRL(ctx)
	if err != nil {
		result = "failed"
		return fmt.Errorf("connecting to CA for %s: %w", crlId, err)
	}

	err = caStream.Send(&capb.GenerateCRLRequest{
		Payload: &capb.GenerateCRLRequest_Metadata{
			Metadata: &capb.CRLMetadata{
				IssuerNameID: int64(issuerNameID),
				ThisUpdate:   atTime.UnixNano(),
				ShardIdx:     int64(shardIdx),
			},
		},
	})
	if err != nil {
		result = "failed"
		return fmt.Errorf("sending CA metadata for %s: %w", crlId, err)
	}

	for {
		entry, err := saStream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			result = "failed"
			return fmt.Errorf("retrieving entry from SA for %s: %w", crlId, err)
		}

		err = caStream.Send(&capb.GenerateCRLRequest{
			Payload: &capb.GenerateCRLRequest_Entry{
				Entry: entry,
			},
		})
		if err != nil {
			result = "failed"
			return fmt.Errorf("sending entry to CA for %s: %w", crlId, err)
		}
	}

	// It's okay to close the CA send stream before we start reading from the
	// receive stream, because we know that the CA has to hold the entire tbsCRL
	// in memory before it can sign it and start returning the real CRL.
	err = caStream.CloseSend()
	if err != nil {
		result = "failed"
		return fmt.Errorf("closing CA request stream for %s: %w", crlId, err)
	}

	csStream, err := cu.cs.UploadCRL(ctx)
	if err != nil {
		result = "failed"
		return fmt.Errorf("connecting to CRLStorer for %s: %w", crlId, err)
	}

	err = csStream.Send(&cspb.UploadCRLRequest{
		Payload: &cspb.UploadCRLRequest_Metadata{
			Metadata: &cspb.CRLMetadata{
				IssuerNameID: int64(issuerNameID),
				Number:       atTime.UnixNano(),
				ShardIdx:     int64(shardIdx),
			},
		},
	})
	if err != nil {
		result = "failed"
		return fmt.Errorf("sending CRLStorer metadata for %s: %w", crlId, err)
	}

	crlLen := 0
	crlHash := sha256.New()
	for {
		out, err := caStream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			result = "failed"
			return fmt.Errorf("receiving CRL bytes for %s: %w", crlId, err)
		}

		err = csStream.Send(&cspb.UploadCRLRequest{
			Payload: &cspb.UploadCRLRequest_CrlChunk{
				CrlChunk: out.Chunk,
			},
		})
		if err != nil {
			result = "failed"
			return fmt.Errorf("uploading CRL bytes for %s: %w", crlId, err)
		}

		crlLen += len(out.Chunk)
		crlHash.Write(out.Chunk)
	}

	cu.log.Infof("Generated CRL: id=[%s] size=[%d] hash=[%x]", crlId, crlLen, crlHash.Sum(nil))

	_, err = csStream.CloseAndRecv()
	if err != nil {
		result = "failed"
		return fmt.Errorf("closing CRLStorer upload stream for %s: %w", crlId, err)
	}

	return nil
}

// getShardBoundaries computes the start (inclusive) and end (exclusive) times
// for a given integer-indexed CRL shard. The idea here is that shards should be
// stable. Picture a timeline, divided into chunks. Number those chunks from 0
// to cu.numShards, then repeat the cycle when you run out of numbers:
//
//	   chunk:  5     0     1     2     3     4     5     0     1     2     3
//	...-----|-----|-----|-----|-----|-----|-----|-----|-----|-----|-----|-----...
//	                         ^  ^-atTime                         ^
//	   atTime-lookbackPeriod-┘          atTime+lookforwardPeriod-┘
//
// The width of each chunk is determined by dividing the total time window we
// care about (lookbackPeriod+lookforwardPeriod) by the number of shards we
// want (numShards).
//
// Even as "now" (atTime) moves forward, and the total window of expiration
// times that we care about moves forward, the boundaries of each chunk remain
// stable:
//
//	   chunk:  5     0     1     2     3     4     5     0     1     2     3
//	...-----|-----|-----|-----|-----|-----|-----|-----|-----|-----|-----|-----...
//	                                 ^  ^-atTime                         ^
//	           atTime-lookbackPeriod-┘          atTime+lookforwardPeriod-┘
//
// However, note that at essentially all times the window includes parts of two
// different instances of the chunk which appears at its ends. For example,
// in the second diagram above, the window includes almost all of the middle
// chunk labeled "3", but also includes just a little bit of the rightmost chunk
// also labeled "3".
//
// In order to handle this case, this function always treats the *leftmost*
// (i.e. earliest) chunk with the given ID that has *any* overlap with the
// current window as the current shard. It returns the boundaries of this chunk
// as the boundaries of the desired shard. In the diagram below, even though
// there is another chunk with ID "1" near the right-hand edge of the window,
// that chunk is ignored.
//
//	   shard:           |  1  |  2  |  3  |  4  |  5  |  0  |
//	...-----|-----|-----|-----|-----|-----|-----|-----|-----|-----|-----|-----...
//	                         ^  ^-atTime                         ^
//	   atTime-lookbackPeriod-┘          atTime+lookforwardPeriod-┘
//
// This means that the lookforwardPeriod MUST be configured large enough that
// there is a buffer of at least one whole chunk width between the actual
// furthest-future expiration (generally atTime+90d) and the right-hand edge of
// the window (atTime+lookforwardPeriod).
func (cu *crlUpdater) getShardBoundaries(atTime time.Time, shardIdx int) (time.Time, time.Time) {
	// Ensure that the given shard index falls within the space of acceptable indices.
	shardIdx = shardIdx % cu.numShards

	// Compute the width of the full window.
	windowWidth := cu.lookbackPeriod + cu.lookforwardPeriod
	// Compute the amount of time between the left-hand edge of the most recent
	// "0" chunk and the current time.
	atTimeOffset := time.Duration(atTime.Sub(time.Time{}).Nanoseconds() % windowWidth.Nanoseconds())
	// Compute the left-hand edge of the most recent "0" chunk.
	zeroStart := atTime.Add(-atTimeOffset)

	// Compute the width of a single shard.
	shardWidth := time.Duration(windowWidth.Nanoseconds() / int64(cu.numShards))
	// Compute the amount of time between the left-hand edge of the most recent
	// "0" chunk and the left-hand edge of the desired chunk.
	shardOffset := time.Duration(int64(shardIdx) * shardWidth.Nanoseconds())
	// Compute the left-hand edge of the most recent chunk with the given index.
	shardStart := zeroStart.Add(shardOffset)
	// Compute the right-hand edge of the most recent chunk with the given index.
	shardEnd := shardStart.Add(shardWidth)

	// But the shard boundaries we just computed might be for a chunk that is
	// completely behind the left-hand edge of our current window. If they are,
	// bump them forward by one window width to bring them inside our window.
	if shardEnd.Before(atTime.Add(-cu.lookbackPeriod)) {
		shardStart = shardStart.Add(windowWidth)
		shardEnd = shardEnd.Add(windowWidth)
	}
	return shardStart, shardEnd
}
