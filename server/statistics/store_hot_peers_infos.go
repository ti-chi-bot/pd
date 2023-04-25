// Copyright 2019 TiKV Project Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package statistics

import (
	"fmt"
	"math"

	"github.com/tikv/pd/server/core"
)

// StoreHotPeersInfos is used to get human-readable description for hot regions.
// NOTE: This type is exported by HTTP API. Please pay more attention when modifying it.
type StoreHotPeersInfos struct {
	AsPeer   StoreHotPeersStat `json:"as_peer"`
	AsLeader StoreHotPeersStat `json:"as_leader"`
}

// StoreHotPeersStat is used to record the hot region statistics group by store.
// NOTE: This type is exported by HTTP API. Please pay more attention when modifying it.
type StoreHotPeersStat map[uint64]*HotPeersStat

// CollectHotPeerInfos only returns TotalBytesRate,TotalKeysRate,TotalQueryRate,Count
func CollectHotPeerInfos(stores []*core.StoreInfo, regionStats map[uint64][]*HotPeerStat) *StoreHotPeersInfos {
	peerLoadSum := make([]float64, DimLen)
	collect := func(kind core.ResourceKind) StoreHotPeersStat {
		ret := make(StoreHotPeersStat, len(stores))
		for _, store := range stores {
			id := store.GetID()
			hotPeers, ok := regionStats[id]
			if !ok {
				continue
			}
			for i := range peerLoadSum {
				peerLoadSum[i] = 0
			}
			peers := filterHotPeers(kind, hotPeers)
			for _, peer := range peers {
				for j := range peerLoadSum {
					peerLoadSum[j] += peer.GetLoad(j)
				}
			}
			ret[id] = &HotPeersStat{
				TotalBytesRate: peerLoadSum[ByteDim],
				TotalKeysRate:  peerLoadSum[KeyDim],
				TotalQueryRate: peerLoadSum[QueryDim],
				Count:          len(peers),
			}
		}
		return ret
	}
	return &StoreHotPeersInfos{
		AsPeer:   collect(core.RegionKind),
		AsLeader: collect(core.LeaderKind),
	}
}

// GetHotStatus returns the hot status for a given type.
// NOTE: This function is exported by HTTP API. It does not contain `isLearner` and `LastUpdateTime` field. If need, please call `updateRegionInfo`.
func GetHotStatus(stores []*core.StoreInfo, storesLoads map[uint64][]float64, regionStats map[uint64][]*HotPeerStat, typ RWType, isTraceRegionFlow bool) *StoreHotPeersInfos {
	stInfos := SummaryStoreInfos(stores)
	stLoadInfosAsLeader := SummaryStoresLoad(
		stInfos,
		storesLoads,
		nil,
		regionStats,
		isTraceRegionFlow,
		typ, core.LeaderKind)
	stLoadInfosAsPeer := SummaryStoresLoad(
		stInfos,
		storesLoads,
		nil,
		regionStats,
		isTraceRegionFlow,
		typ, core.RegionKind)

	asLeader := make(StoreHotPeersStat, len(stLoadInfosAsLeader))
	asPeer := make(StoreHotPeersStat, len(stLoadInfosAsPeer))

	for id, detail := range stLoadInfosAsLeader {
		asLeader[id] = detail.ToHotPeersStat()
	}
	for id, detail := range stLoadInfosAsPeer {
		asPeer[id] = detail.ToHotPeersStat()
	}
	return &StoreHotPeersInfos{
		AsLeader: asLeader,
		AsPeer:   asPeer,
	}
}

// SummaryStoresLoad Load information of all available stores.
// it will filter the hot peer and calculate the current and future stat(rate,count) for each store
func SummaryStoresLoad(
	storeInfos map[uint64]*StoreSummaryInfo,
	storesLoads map[uint64][]float64,
	storesHistoryLoads *StoreHistoryLoads,
	storeHotPeers map[uint64][]*HotPeerStat,
	isTraceRegionFlow bool,
	rwTy RWType,
	kind core.ResourceKind,
) map[uint64]*StoreLoadDetail {
	// loadDetail stores the storeID -> hotPeers stat and its current and future stat(rate,count)
	loadDetail := make(map[uint64]*StoreLoadDetail, len(storesLoads))

	tikvLoadDetail := summaryStoresLoadByEngine(
		storeInfos,
		storesLoads,
		storesHistoryLoads,
		storeHotPeers,
		rwTy, kind,
		newTikvCollector(),
	)
	tiflashLoadDetail := summaryStoresLoadByEngine(
		storeInfos,
		storesLoads,
		storesHistoryLoads,
		storeHotPeers,
		rwTy, kind,
		newTiFlashCollector(isTraceRegionFlow),
	)

	for _, detail := range append(tikvLoadDetail, tiflashLoadDetail...) {
		loadDetail[detail.GetID()] = detail
	}
	return loadDetail
}

func summaryStoresLoadByEngine(
	storeInfos map[uint64]*StoreSummaryInfo,
	storesLoads map[uint64][]float64,
	storesHistoryLoads *StoreHistoryLoads,
	storeHotPeers map[uint64][]*HotPeerStat,
	rwTy RWType,
	kind core.ResourceKind,
	collector storeCollector,
) []*StoreLoadDetail {
	loadDetail := make([]*StoreLoadDetail, 0, len(storeInfos))
	allStoreLoadSum := make([]float64, DimLen)
	allStoreHistoryLoadSum := make([][]float64, DimLen)
	allStoreCount := 0
	allHotPeersCount := 0

	for _, info := range storeInfos {
		store := info.StoreInfo
		id := store.GetID()
		storeLoads, ok := storesLoads[id]
		if !ok || !collector.Filter(info, kind) {
			continue
		}

		// Find all hot peers first
		var hotPeers []*HotPeerStat
		peerLoadSum := make([]float64, DimLen)
		// TODO: To remove `filterHotPeers`, we need to:
		// HotLeaders consider `Write{Bytes,Keys}`, so when we schedule `writeLeader`, all peers are leader.
		for _, peer := range filterHotPeers(kind, storeHotPeers[id]) {
			for i := range peerLoadSum {
				peerLoadSum[i] += peer.GetLoad(i)
			}
			hotPeers = append(hotPeers, peer.Clone())
		}
		{
			// Metric for debug.
			ty := "byte-rate-" + rwTy.String() + "-" + kind.String()
			hotPeerSummary.WithLabelValues(ty, fmt.Sprintf("%v", id)).Set(peerLoadSum[ByteDim])
			ty = "key-rate-" + rwTy.String() + "-" + kind.String()
			hotPeerSummary.WithLabelValues(ty, fmt.Sprintf("%v", id)).Set(peerLoadSum[KeyDim])
			ty = "query-rate-" + rwTy.String() + "-" + kind.String()
			hotPeerSummary.WithLabelValues(ty, fmt.Sprintf("%v", id)).Set(peerLoadSum[QueryDim])
		}
		loads := collector.GetLoads(storeLoads, peerLoadSum, rwTy, kind)

		var historyLoads [][]float64
		if storesHistoryLoads != nil {
			historyLoads = storesHistoryLoads.Get(id, rwTy, kind)
			for i, loads := range historyLoads {
				if allStoreHistoryLoadSum[i] == nil || len(allStoreHistoryLoadSum[i]) < len(loads) {
					allStoreHistoryLoadSum[i] = make([]float64, len(loads))
				}
				for j, load := range loads {
					allStoreHistoryLoadSum[i][j] += load
				}
			}
			storesHistoryLoads.Add(id, rwTy, kind, loads)
		}

		for i := range allStoreLoadSum {
			allStoreLoadSum[i] += loads[i]
		}
		allStoreCount += 1
		allHotPeersCount += len(hotPeers)

		// Build store load prediction from current load and pending influence.
		stLoadPred := (&StoreLoad{
			Loads:        loads,
			Count:        float64(len(hotPeers)),
			HistoryLoads: historyLoads,
		}).ToLoadPred(rwTy, info.PendingSum)

		// Construct store load info.
		loadDetail = append(loadDetail, &StoreLoadDetail{
			StoreSummaryInfo: info,
			LoadPred:         stLoadPred,
			HotPeers:         hotPeers,
		})
	}

	if allStoreCount == 0 {
		return loadDetail
	}

	expectCount := float64(allHotPeersCount) / float64(allStoreCount)
	expectLoads := make([]float64, len(allStoreLoadSum))
	for i := range expectLoads {
		expectLoads[i] = allStoreLoadSum[i] / float64(allStoreCount)
	}

	// todo: remove some the max value or min value to avoid the effect of extreme value.
	expectHistoryLoads := make([][]float64, DimLen)
	for i := range allStoreHistoryLoadSum {
		expectHistoryLoads[i] = make([]float64, len(allStoreHistoryLoadSum[i]))
		for j := range allStoreHistoryLoadSum[i] {
			expectHistoryLoads[i][j] = allStoreHistoryLoadSum[i][j] / float64(allStoreCount)
		}
	}
	stddevLoads := make([]float64, len(allStoreLoadSum))
	if allHotPeersCount != 0 {
		for _, detail := range loadDetail {
			for i := range expectLoads {
				stddevLoads[i] += math.Pow(detail.LoadPred.Current.Loads[i]-expectLoads[i], 2)
			}
		}
		for i := range stddevLoads {
			stddevLoads[i] = math.Sqrt(stddevLoads[i]/float64(allStoreCount)) / expectLoads[i]
		}
	}

	{
		// Metric for debug.
		engine := collector.Engine()
		ty := "exp-byte-rate-" + rwTy.String() + "-" + kind.String()
		hotPeerSummary.WithLabelValues(ty, engine).Set(expectLoads[ByteDim])
		ty = "exp-key-rate-" + rwTy.String() + "-" + kind.String()
		hotPeerSummary.WithLabelValues(ty, engine).Set(expectLoads[KeyDim])
		ty = "exp-query-rate-" + rwTy.String() + "-" + kind.String()
		hotPeerSummary.WithLabelValues(ty, engine).Set(expectLoads[QueryDim])
		ty = "exp-count-rate-" + rwTy.String() + "-" + kind.String()
		hotPeerSummary.WithLabelValues(ty, engine).Set(expectCount)
		ty = "stddev-byte-rate-" + rwTy.String() + "-" + kind.String()
		hotPeerSummary.WithLabelValues(ty, engine).Set(stddevLoads[ByteDim])
		ty = "stddev-key-rate-" + rwTy.String() + "-" + kind.String()
		hotPeerSummary.WithLabelValues(ty, engine).Set(stddevLoads[KeyDim])
		ty = "stddev-query-rate-" + rwTy.String() + "-" + kind.String()
		hotPeerSummary.WithLabelValues(ty, engine).Set(stddevLoads[QueryDim])
	}
	expect := StoreLoad{
		Loads:        expectLoads,
		Count:        expectCount,
		HistoryLoads: expectHistoryLoads,
	}
	stddev := StoreLoad{
		Loads: stddevLoads,
		Count: expectCount,
	}
	for _, detail := range loadDetail {
		detail.LoadPred.Expect = expect
		detail.LoadPred.Stddev = stddev
	}
	return loadDetail
}

func filterHotPeers(kind core.ResourceKind, peers []*HotPeerStat) []*HotPeerStat {
	ret := make([]*HotPeerStat, 0, len(peers))
	for _, peer := range peers {
		if kind != core.LeaderKind || peer.IsLeader() {
			ret = append(ret, peer)
		}
	}
	return ret
}
