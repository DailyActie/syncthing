// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"sort"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/model"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/upgrade"
	"github.com/thejerf/suture"
)

// Current version number of the usage report, for acceptance purposes. If
// fields are added or changed this integer must be incremented so that users
// are prompted for acceptance of the new report.
const usageReportVersion = 2

type usageReportingManager struct {
	cfg   *config.Wrapper
	model *model.Model
	sup   *suture.Supervisor
}

func newUsageReportingManager(cfg *config.Wrapper, m *model.Model) *usageReportingManager {
	mgr := &usageReportingManager{
		cfg:   cfg,
		model: m,
	}

	// Start UR if it's enabled.
	mgr.CommitConfiguration(config.Configuration{}, cfg.Raw())

	// Listen to future config changes so that we can start and stop as
	// appropriate.
	cfg.Subscribe(mgr)

	return mgr
}

func (m *usageReportingManager) VerifyConfiguration(from, to config.Configuration) error {
	return nil
}

func (m *usageReportingManager) CommitConfiguration(from, to config.Configuration) bool {
	if to.Options.URAccepted >= usageReportVersion && m.sup == nil {
		// Usage reporting was turned on; lets start it.
		svc := newUsageReportingService(m.cfg, m.model)
		m.sup = suture.NewSimple("usageReporting")
		m.sup.Add(svc)
		m.sup.ServeBackground()
	} else if to.Options.URAccepted < usageReportVersion && m.sup != nil {
		// Usage reporting was turned off
		m.sup.Stop()
		m.sup = nil
	}

	return true
}

func (m *usageReportingManager) String() string {
	return fmt.Sprintf("usageReportingManager@%p", m)
}

// reportData returns the data to be sent in a usage report. It's used in
// various places, so not part of the usageReportingSvc object.
func reportData(cfg *config.Wrapper, m *model.Model) map[string]interface{} {
	res := make(map[string]interface{})
	res["urVersion"] = usageReportVersion
	res["uniqueID"] = cfg.Options().URUniqueID
	res["version"] = Version
	res["longVersion"] = LongVersion
	res["platform"] = runtime.GOOS + "-" + runtime.GOARCH
	res["numFolders"] = len(cfg.Folders())
	res["numDevices"] = len(cfg.Devices())

	var totFiles, maxFiles int
	var totBytes, maxBytes int64
	for folderID := range cfg.Folders() {
		files, _, bytes := m.GlobalSize(folderID)
		totFiles += files
		totBytes += bytes
		if files > maxFiles {
			maxFiles = files
		}
		if bytes > maxBytes {
			maxBytes = bytes
		}
	}

	res["totFiles"] = totFiles
	res["folderMaxFiles"] = maxFiles
	res["totMiB"] = totBytes / 1024 / 1024
	res["folderMaxMiB"] = maxBytes / 1024 / 1024

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	res["memoryUsageMiB"] = (mem.Sys - mem.HeapReleased) / 1024 / 1024

	var perf float64
	for i := 0; i < 5; i++ {
		p := cpuBench()
		if p > perf {
			perf = p
		}
	}
	res["sha256Perf"] = perf

	bytes, err := memorySize()
	if err == nil {
		res["memorySize"] = bytes / 1024 / 1024
	}
	res["numCPU"] = runtime.NumCPU()

	var rescanIntvs []int
	folderUses := map[string]int{
		"readonly":      0,
		"ignorePerms":   0,
		"ignoreDelete":  0,
		"autoNormalize": 0,
	}
	for _, cfg := range cfg.Folders() {
		rescanIntvs = append(rescanIntvs, cfg.RescanIntervalS)

		if cfg.ReadOnly {
			folderUses["readonly"]++
		}
		if cfg.IgnorePerms {
			folderUses["ignorePerms"]++
		}
		if cfg.IgnoreDelete {
			folderUses["ignoreDelete"]++
		}
		if cfg.AutoNormalize {
			folderUses["autoNormalize"]++
		}
	}
	sort.Ints(rescanIntvs)
	res["rescanIntvs"] = rescanIntvs
	res["folderUses"] = folderUses

	deviceUses := map[string]int{
		"introducer":       0,
		"customCertName":   0,
		"compressAlways":   0,
		"compressMetadata": 0,
		"compressNever":    0,
		"dynamicAddr":      0,
		"staticAddr":       0,
	}
	for _, cfg := range cfg.Devices() {
		if cfg.Introducer {
			deviceUses["introducer"]++
		}
		if cfg.CertName != "" && cfg.CertName != "syncthing" {
			deviceUses["customCertName"]++
		}
		if cfg.Compression == protocol.CompressAlways {
			deviceUses["compressAlways"]++
		} else if cfg.Compression == protocol.CompressMetadata {
			deviceUses["compressMetadata"]++
		} else if cfg.Compression == protocol.CompressNever {
			deviceUses["compressNever"]++
		}
		for _, addr := range cfg.Addresses {
			if addr == "dynamic" {
				deviceUses["dynamicAddr"]++
			} else {
				deviceUses["staticAddr"]++
			}
		}
	}
	res["deviceUses"] = deviceUses

	defaultAnnounceServersDNS, defaultAnnounceServersIP, otherAnnounceServers := 0, 0, 0
	for _, addr := range cfg.Options().GlobalAnnServers {
		if addr == "default" {
			defaultAnnounceServersDNS++
		} else if stringIn(addr, config.DefaultDiscoveryServersIP) {
			defaultAnnounceServersIP++
		} else {
			otherAnnounceServers++
		}
	}
	res["announce"] = map[string]interface{}{
		"globalEnabled":     cfg.Options().GlobalAnnEnabled,
		"localEnabled":      cfg.Options().LocalAnnEnabled,
		"defaultServersDNS": defaultAnnounceServersDNS,
		"defaultServersIP":  defaultAnnounceServersIP,
		"otherServers":      otherAnnounceServers,
	}

	defaultRelayServers, otherRelayServers := 0, 0
	for _, addr := range cfg.Options().RelayServers {
		switch addr {
		case "dynamic+https://relays.syncthing.net":
			defaultRelayServers++
		default:
			otherRelayServers++
		}
	}
	res["relays"] = map[string]interface{}{
		"enabled":        cfg.Options().RelaysEnabled,
		"defaultServers": defaultRelayServers,
		"otherServers":   otherRelayServers,
	}

	res["usesRateLimit"] = cfg.Options().MaxRecvKbps > 0 || cfg.Options().MaxSendKbps > 0

	res["upgradeAllowedManual"] = !(upgrade.DisabledByCompilation || noUpgrade)
	res["upgradeAllowedAuto"] = !(upgrade.DisabledByCompilation || noUpgrade) && cfg.Options().AutoUpgradeIntervalH > 0

	return res
}

func stringIn(needle string, haystack []string) bool {
	for _, s := range haystack {
		if needle == s {
			return true
		}
	}
	return false
}

type usageReportingService struct {
	cfg   *config.Wrapper
	model *model.Model
	stop  chan struct{}
}

func newUsageReportingService(cfg *config.Wrapper, model *model.Model) *usageReportingService {
	return &usageReportingService{
		cfg:   cfg,
		model: model,
		stop:  make(chan struct{}),
	}
}

func (s *usageReportingService) sendUsageReport() error {
	d := reportData(s.cfg, s.model)
	var b bytes.Buffer
	json.NewEncoder(&b).Encode(d)

	transp := &http.Transport{}
	client := &http.Client{Transport: transp}
	if BuildEnv == "android" {
		// This works around the lack of DNS resolution on Android... :(
		transp.Dial = func(network, addr string) (net.Conn, error) {
			return net.Dial(network, "194.126.249.13:443")
		}
	}

	if s.cfg.Options().URPostInsecurely {
		transp.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}
	_, err := client.Post(s.cfg.Options().URURL, "application/json", &b)
	return err
}

func (s *usageReportingService) Serve() {
	s.stop = make(chan struct{})

	l.Infoln("Starting usage reporting")
	defer l.Infoln("Stopping usage reporting")

	t := time.NewTimer(time.Duration(s.cfg.Options().URInitialDelayS) * time.Second) // time to initial report at start
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			err := s.sendUsageReport()
			if err != nil {
				l.Infoln("Usage report:", err)
			}
			t.Reset(24 * time.Hour) // next report tomorrow
		}
	}
}

func (s *usageReportingService) Stop() {
	close(s.stop)
}

// cpuBench returns CPU performance as a measure of single threaded SHA-256 MiB/s
func cpuBench() float64 {
	chunkSize := 100 * 1 << 10
	h := sha256.New()
	bs := make([]byte, chunkSize)
	rand.Reader.Read(bs)

	t0 := time.Now()
	b := 0
	for time.Since(t0) < 125*time.Millisecond {
		h.Write(bs)
		b += chunkSize
	}
	h.Sum(nil)
	d := time.Since(t0)
	return float64(int(float64(b)/d.Seconds()/(1<<20)*100)) / 100
}
