/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2024 Red Hat, Inc.
 *
 */

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	vmschema "kubevirt.io/api/core/v1"

	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/cmd/sidecars/network-bridge-binding/callback"
	"kubevirt.io/kubevirt/cmd/sidecars/network-bridge-binding/domain"

	hooksInfo "kubevirt.io/kubevirt/pkg/hooks/info"
	hooksV1alpha3 "kubevirt.io/kubevirt/pkg/hooks/v1alpha3"
)

type InfoServer struct {
	Version string
}

type MacHelper interface {
	GenerateMac(instance *vmschema.VirtualMachineInstance) net.HardwareAddr
}

func (s InfoServer) Info(_ context.Context, _ *hooksInfo.InfoParams) (*hooksInfo.InfoResult, error) {
	return &hooksInfo.InfoResult{
		Name: "network-bridge-binding",
		Versions: []string{
			s.Version,
		},
		HookPoints: []*hooksInfo.HookPoint{
			{
				Name:     hooksInfo.OnDefineDomainHookPointName,
				Priority: 0,
			},
			{
				Name:     hooksInfo.ShutdownHookPointName,
				Priority: 0,
			},
		},
	}, nil
}

type V1alpha3Server struct {
	Done      chan struct{}
	Mac       chan string
	MacHelper MacHelper
}

func (s V1alpha3Server) OnDefineDomain(_ context.Context, params *hooksV1alpha3.OnDefineDomainParams) (*hooksV1alpha3.OnDefineDomainResult, error) {
	var vmiMac string

	vmi := &vmschema.VirtualMachineInstance{}

	if err := json.Unmarshal(params.GetVmi(), vmi); err != nil {
		return nil, fmt.Errorf("failed to unmarshal VMI: %v", err)
	}

	useVirtioTransitional := vmi.Spec.Domain.Devices.UseVirtioTransitional != nil && *vmi.Spec.Domain.Devices.UseVirtioTransitional

	opts := domain.NetworkConfiguratorOptions{
		UseVirtioTransitional: useVirtioTransitional,
	}

	if vmiMac = vmi.Spec.Domain.Devices.Interfaces[0].MacAddress; vmiMac == "" {
		vmiMac = s.MacHelper.GenerateMac(vmi).String()
		opts.Mac = vmiMac
		log.Log.Infof("Evaluated VMI mac: %s", vmiMac)
	}

	select {
	case s.Mac <- vmiMac:
		log.Log.Infof("Sent MAC address to DHCPd: %s", vmiMac)
	default:
		log.Log.Errorf("Failed to send MAC address to DHCPd: %s", vmiMac)
	}

	bridgeConfigurator, err := domain.NewBridgeNetworkConfigurator(vmi.Spec.Domain.Devices.Interfaces, vmi.Spec.Networks, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to create bridge configurator: %v", err)
	}

	newDomainXML, err := callback.OnDefineDomain(params.GetDomainXML(), bridgeConfigurator)
	if err != nil {
		return nil, err
	}

	return &hooksV1alpha3.OnDefineDomainResult{
		DomainXML: newDomainXML,
	}, nil
}

func (s V1alpha3Server) PreCloudInitIso(_ context.Context, params *hooksV1alpha3.PreCloudInitIsoParams) (*hooksV1alpha3.PreCloudInitIsoResult, error) {
	return &hooksV1alpha3.PreCloudInitIsoResult{
		CloudInitData: params.GetCloudInitData(),
	}, nil
}

func (s V1alpha3Server) Shutdown(_ context.Context, _ *hooksV1alpha3.ShutdownParams) (*hooksV1alpha3.ShutdownResult, error) {
	log.Log.Info("Shutdown bridge network binding")
	s.Done <- struct{}{}
	return &hooksV1alpha3.ShutdownResult{}, nil
}

func waitForShutdown(server *grpc.Server, errChan <-chan error, shutdownChan <-chan struct{}) {
	// Handle signals to properly shutdown process
	signalStopChan := make(chan os.Signal, 1)
	signal.Notify(signalStopChan, os.Interrupt,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	)
	var err error
	select {
	case s := <-signalStopChan:
		log.Log.Infof("bridge sidecar received signal: %s", s.String())
	case err = <-errChan:
		log.Log.Reason(err).Error("Failed to run grpc server")
	case <-shutdownChan:
		log.Log.Info("Exiting")
	}

	if err == nil {
		server.GracefulStop()
	}
}

func Serve(server *grpc.Server, socket net.Listener, shutdownChan <-chan struct{}) {
	errChan := make(chan error)
	go func() {
		errChan <- server.Serve(socket)
	}()

	waitForShutdown(server, errChan, shutdownChan)
}
