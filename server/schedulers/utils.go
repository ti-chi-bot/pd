// Copyright 2017 TiKV Project Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package schedulers

import (
	"math"
	"net/url"
	"strconv"
	"time"

	"github.com/montanaflynn/stats"
	"github.com/pingcap/log"
	"github.com/tikv/pd/pkg/errs"
	"github.com/tikv/pd/pkg/typeutil"
	"github.com/tikv/pd/server/core"
	"github.com/tikv/pd/server/schedule"
	"github.com/tikv/pd/server/schedule/operator"
	"github.com/tikv/pd/server/schedule/opt"
	"github.com/tikv/pd/server/statistics"
	"go.uber.org/zap"
)

const (
	// adjustRatio is used to adjust TolerantSizeRatio according to region count.
	adjustRatio                  float64 = 0.005
	leaderTolerantSizeRatio      float64 = 5.0
	minTolerantSizeRatio         float64 = 1.0
	defaultMinRetryLimit                 = 1
	defaultRetryQuotaAttenuation         = 2
)

func shouldBalance(cluster opt.Cluster, source, target *core.StoreInfo, region *core.RegionInfo, kind core.ScheduleKind, opInfluence operator.OpInfluence, scheduleName string) (shouldBalance bool, sourceScore float64, targetScore float64) {
	// The reason we use max(regionSize, averageRegionSize) to check is:
	// 1. prevent moving small regions between stores with close scores, leading to unnecessary balance.
	// 2. prevent moving huge regions, leading to over balance.
	sourceID := source.GetID()
	targetID := target.GetID()
	tolerantResource := getTolerantResource(cluster, region, kind)
	sourceInfluence := opInfluence.GetStoreInfluence(sourceID).ResourceProperty(kind)
	targetInfluence := opInfluence.GetStoreInfluence(targetID).ResourceProperty(kind)
	sourceDelta, targetDelta := sourceInfluence-tolerantResource, targetInfluence+tolerantResource
	opts := cluster.GetOpts()
	switch kind.Resource {
	case core.LeaderKind:
		sourceScore = source.LeaderScore(kind.Policy, sourceDelta)
		targetScore = target.LeaderScore(kind.Policy, targetDelta)
	case core.RegionKind:
		sourceScore = source.RegionScore(opts.GetRegionScoreFormulaVersion(), opts.GetHighSpaceRatio(), opts.GetLowSpaceRatio(), sourceDelta, -1)
		targetScore = target.RegionScore(opts.GetRegionScoreFormulaVersion(), opts.GetHighSpaceRatio(), opts.GetLowSpaceRatio(), targetDelta, 1)
	}
	if opts.IsDebugMetricsEnabled() {
		opInfluenceStatus.WithLabelValues(scheduleName, strconv.FormatUint(sourceID, 10), "source").Set(float64(sourceInfluence))
		opInfluenceStatus.WithLabelValues(scheduleName, strconv.FormatUint(targetID, 10), "target").Set(float64(targetInfluence))
		tolerantResourceStatus.WithLabelValues(scheduleName, strconv.FormatUint(sourceID, 10), strconv.FormatUint(targetID, 10)).Set(float64(tolerantResource))
	}
	// Make sure after move, source score is still greater than target score.
	shouldBalance = sourceScore > targetScore

	if !shouldBalance {
		log.Debug("skip balance "+kind.Resource.String(),
			zap.String("scheduler", scheduleName), zap.Uint64("region-id", region.GetID()), zap.Uint64("source-store", sourceID), zap.Uint64("target-store", targetID),
			zap.Int64("source-size", source.GetRegionSize()), zap.Float64("source-score", sourceScore),
			zap.Int64("source-influence", sourceInfluence),
			zap.Int64("target-size", target.GetRegionSize()), zap.Float64("target-score", targetScore),
			zap.Int64("target-influence", targetInfluence),
			zap.Int64("average-region-size", cluster.GetAverageRegionSize()),
			zap.Int64("tolerant-resource", tolerantResource))
	}
	return shouldBalance, sourceScore, targetScore
}

func getTolerantResource(cluster opt.Cluster, region *core.RegionInfo, kind core.ScheduleKind) int64 {
	tolerantSizeRatio := adjustTolerantRatio(cluster)
	if kind.Resource == core.LeaderKind && kind.Policy == core.ByCount {
		if tolerantSizeRatio == 0 {
			tolerantSizeRatio = leaderTolerantSizeRatio
		}
		leaderCount := int64(1.0 * tolerantSizeRatio)
		return leaderCount
	}

	regionSize := region.GetApproximateSize()
	if regionSize < cluster.GetAverageRegionSize() {
		regionSize = cluster.GetAverageRegionSize()
	}
	regionSize = int64(float64(regionSize) * tolerantSizeRatio)
	return regionSize
}

func adjustTolerantRatio(cluster opt.Cluster) float64 {
	var tolerantSizeRatio float64
	switch c := cluster.(type) {
	case *schedule.RangeCluster:
		// range cluster use a separate configuration
		tolerantSizeRatio = c.GetTolerantSizeRatio()
	default:
		tolerantSizeRatio = cluster.GetOpts().GetTolerantSizeRatio()
	}
	if tolerantSizeRatio == 0 {
		var maxRegionCount float64
		stores := cluster.GetStores()
		for _, store := range stores {
			regionCount := float64(cluster.GetStoreRegionCount(store.GetID()))
			if maxRegionCount < regionCount {
				maxRegionCount = regionCount
			}
		}
		tolerantSizeRatio = maxRegionCount * adjustRatio
		if tolerantSizeRatio < minTolerantSizeRatio {
			tolerantSizeRatio = minTolerantSizeRatio
		}
	}
	return tolerantSizeRatio
}

func adjustBalanceLimit(cluster opt.Cluster, kind core.ResourceKind) uint64 {
	stores := cluster.GetStores()
	counts := make([]float64, 0, len(stores))
	for _, s := range stores {
		if s.IsUp() {
			counts = append(counts, float64(s.ResourceCount(kind)))
		}
	}
	limit, _ := stats.StandardDeviation(counts)
	return typeutil.MaxUint64(1, uint64(limit))
}

func getKeyRanges(args []string) ([]core.KeyRange, error) {
	var ranges []core.KeyRange
	for len(args) > 1 {
		startKey, err := url.QueryUnescape(args[0])
		if err != nil {
			return nil, errs.ErrQueryUnescape.Wrap(err).FastGenWithCause()
		}
		endKey, err := url.QueryUnescape(args[1])
		if err != nil {
			return nil, errs.ErrQueryUnescape.Wrap(err).FastGenWithCause()
		}
		args = args[2:]
		ranges = append(ranges, core.NewKeyRange(startKey, endKey))
	}
	if len(ranges) == 0 {
		return []core.KeyRange{core.NewKeyRange("", "")}, nil
	}
	return ranges, nil
}

// Influence records operator influence.
type Influence struct {
	ByteRate float64
	KeyRate  float64
	Count    float64
}

func (infl Influence) add(rhs *Influence, w float64) Influence {
	infl.ByteRate += rhs.ByteRate * w
	infl.KeyRate += rhs.KeyRate * w
	infl.Count += rhs.Count * w
	return infl
}

// TODO: merge it into OperatorInfluence.
type pendingInfluence struct {
	op                *operator.Operator
	from, to          uint64
	origin            Influence
	maxZombieDuration time.Duration
}

func newPendingInfluence(op *operator.Operator, from, to uint64, infl Influence, maxZombieDur time.Duration) *pendingInfluence {
	return &pendingInfluence{
		op:                op,
		from:              from,
		to:                to,
		origin:            infl,
		maxZombieDuration: maxZombieDur,
	}
}

type storeLoad struct {
	ByteRate float64
	KeyRate  float64
	Count    float64
}

func (load *storeLoad) ToLoadPred(infl Influence) *storeLoadPred {
	future := *load
	future.ByteRate += infl.ByteRate
	future.KeyRate += infl.KeyRate
	future.Count += infl.Count
	return &storeLoadPred{
		Current: *load,
		Future:  future,
	}
}

func stLdByteRate(ld *storeLoad) float64 {
	return ld.ByteRate
}

func stLdKeyRate(ld *storeLoad) float64 {
	return ld.KeyRate
}

func stLdCount(ld *storeLoad) float64 {
	return ld.Count
}

type storeLoadCmp func(ld1, ld2 *storeLoad) int

func negLoadCmp(cmp storeLoadCmp) storeLoadCmp {
	return func(ld1, ld2 *storeLoad) int {
		return -cmp(ld1, ld2)
	}
}

func sliceLoadCmp(cmps ...storeLoadCmp) storeLoadCmp {
	return func(ld1, ld2 *storeLoad) int {
		for _, cmp := range cmps {
			if r := cmp(ld1, ld2); r != 0 {
				return r
			}
		}
		return 0
	}
}

func stLdRankCmp(dim func(ld *storeLoad) float64, rank func(value float64) int64) storeLoadCmp {
	return func(ld1, ld2 *storeLoad) int {
		return rankCmp(dim(ld1), dim(ld2), rank)
	}
}

func rankCmp(a, b float64, rank func(value float64) int64) int {
	aRk, bRk := rank(a), rank(b)
	if aRk < bRk {
		return -1
	} else if aRk > bRk {
		return 1
	}
	return 0
}

// store load prediction
type storeLoadPred struct {
	Current storeLoad
	Future  storeLoad
	Expect  storeLoad
}

func (lp *storeLoadPred) min() *storeLoad {
	return minLoad(&lp.Current, &lp.Future)
}

func (lp *storeLoadPred) max() *storeLoad {
	return maxLoad(&lp.Current, &lp.Future)
}

func (lp *storeLoadPred) diff() *storeLoad {
	mx, mn := lp.max(), lp.min()
	return &storeLoad{
		ByteRate: mx.ByteRate - mn.ByteRate,
		KeyRate:  mx.KeyRate - mn.KeyRate,
		Count:    mx.Count - mn.Count,
	}
}

type storeLPCmp func(lp1, lp2 *storeLoadPred) int

func sliceLPCmp(cmps ...storeLPCmp) storeLPCmp {
	return func(lp1, lp2 *storeLoadPred) int {
		for _, cmp := range cmps {
			if r := cmp(lp1, lp2); r != 0 {
				return r
			}
		}
		return 0
	}
}

func minLPCmp(ldCmp storeLoadCmp) storeLPCmp {
	return func(lp1, lp2 *storeLoadPred) int {
		return ldCmp(lp1.min(), lp2.min())
	}
}

func maxLPCmp(ldCmp storeLoadCmp) storeLPCmp {
	return func(lp1, lp2 *storeLoadPred) int {
		return ldCmp(lp1.max(), lp2.max())
	}
}

func diffCmp(ldCmp storeLoadCmp) storeLPCmp {
	return func(lp1, lp2 *storeLoadPred) int {
		return ldCmp(lp1.diff(), lp2.diff())
	}
}

func minLoad(a, b *storeLoad) *storeLoad {
	return &storeLoad{
		ByteRate: math.Min(a.ByteRate, b.ByteRate),
		KeyRate:  math.Min(a.KeyRate, b.KeyRate),
		Count:    math.Min(a.Count, b.Count),
	}
}

func maxLoad(a, b *storeLoad) *storeLoad {
	return &storeLoad{
		ByteRate: math.Max(a.ByteRate, b.ByteRate),
		KeyRate:  math.Max(a.KeyRate, b.KeyRate),
		Count:    math.Max(a.Count, b.Count),
	}
}

type storeLoadDetail struct {
	Store    *core.StoreInfo
	LoadPred *storeLoadPred
	HotPeers []*statistics.HotPeerStat
}

func (li *storeLoadDetail) toHotPeersStat() *statistics.HotPeersStat {
	peers := make([]statistics.HotPeerStat, 0, len(li.HotPeers))
	var totalBytesRate, totalKeysRate float64
	for _, peer := range li.HotPeers {
		if peer.HotDegree > 0 {
			peers = append(peers, *peer.Clone())
			totalBytesRate += peer.ByteRate
			totalKeysRate += peer.KeyRate
		}
	}
	return &statistics.HotPeersStat{
		TotalBytesRate: math.Round(totalBytesRate),
		TotalKeysRate:  math.Round(totalKeysRate),
		Count:          len(peers),
		Stats:          peers,
	}
}

type retryQuota struct {
	initialLimit int
	minLimit     int
	attenuation  int

	limits map[uint64]int
}

func newRetryQuota(initialLimit, minLimit, attenuation int) *retryQuota {
	return &retryQuota{
		initialLimit: initialLimit,
		minLimit:     minLimit,
		attenuation:  attenuation,
		limits:       make(map[uint64]int),
	}
}

func (q *retryQuota) GetLimit(store *core.StoreInfo) int {
	id := store.GetID()
	if limit, ok := q.limits[id]; ok {
		return limit
	}
	q.limits[id] = q.initialLimit
	return q.initialLimit
}

func (q *retryQuota) ResetLimit(store *core.StoreInfo) {
	q.limits[store.GetID()] = q.initialLimit
}

func (q *retryQuota) Attenuate(store *core.StoreInfo) {
	newLimit := q.GetLimit(store) / q.attenuation
	if newLimit < q.minLimit {
		newLimit = q.minLimit
	}
	q.limits[store.GetID()] = newLimit
}

func (q *retryQuota) GC(keepStores []*core.StoreInfo) {
	set := make(map[uint64]struct{}, len(keepStores))
	for _, store := range keepStores {
		set[store.GetID()] = struct{}{}
	}
	for id := range q.limits {
		if _, ok := set[id]; !ok {
			delete(q.limits, id)
		}
	}
}
