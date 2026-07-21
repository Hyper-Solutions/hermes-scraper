package hermes

import (
	"fmt"
	"sync"
	"sync/atomic"
)

type TelemetrySnapshot struct {
	DataDomeInterstitialCalls uint64
	DataDomeSliderCalls       uint64
	DataDomeTagsCalls         uint64
	BandwidthBytes            int64
}

var (
	trackedClients sync.Map

	totalDataDomeInterstitialCalls atomic.Uint64
	totalDataDomeSliderCalls       atomic.Uint64
	totalDataDomeTagsCalls         atomic.Uint64
	retiredBandwidthBytes          atomic.Int64
	retiredSessionCount            atomic.Uint64
)

func GetTelemetrySnapshot() TelemetrySnapshot {
	snapshot := TelemetrySnapshot{
		DataDomeInterstitialCalls: totalDataDomeInterstitialCalls.Load(),
		DataDomeSliderCalls:       totalDataDomeSliderCalls.Load(),
		DataDomeTagsCalls:         totalDataDomeTagsCalls.Load(),
		BandwidthBytes:            retiredBandwidthBytes.Load(),
	}

	trackedClients.Range(func(key any, _ any) bool {
		client, ok := key.(*wrappedClient)
		if !ok {
			return true
		}

		snapshot.BandwidthBytes += client.bandwidthBytes()
		return true
	})

	return snapshot
}

func FormatBandwidth(bytes int64) string {
	if bytes < 0 {
		bytes = 0
	}

	const (
		megabyte = 1024 * 1024
		gigabyte = 1024 * megabyte
	)

	if bytes >= gigabyte {
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(gigabyte))
	}
	return fmt.Sprintf("%.2f MB", float64(bytes)/float64(megabyte))
}

func registerClient(client *wrappedClient) {
	if client == nil {
		return
	}
	trackedClients.Store(client, struct{}{})
}

func retireClient(client *wrappedClient) {
	if client == nil {
		return
	}

	if _, loaded := trackedClients.LoadAndDelete(client); !loaded {
		return
	}

	retiredBandwidthBytes.Add(client.bandwidthBytes())
	retiredSessionCount.Add(1)
}

func recordDataDomeInterstitialCall(client *wrappedClient) {
	if client != nil {
		client.datadomeInterstitialCalls.Add(1)
	}
	totalDataDomeInterstitialCalls.Add(1)
}

func recordDataDomeSliderCall(client *wrappedClient) {
	if client != nil {
		client.datadomeSliderCalls.Add(1)
	}
	totalDataDomeSliderCalls.Add(1)
}

func recordDataDomeTagsCall(client *wrappedClient) {
	if client != nil {
		client.datadomeTagsCalls.Add(1)
	}
	totalDataDomeTagsCalls.Add(1)
}

func (client *wrappedClient) bandwidthBytes() int64 {
	if client == nil || client.Client == nil {
		return 0
	}

	tracker := client.Client.GetBandwidthTracker()
	if tracker == nil {
		return 0
	}

	return tracker.GetTotalBandwidth()
}
