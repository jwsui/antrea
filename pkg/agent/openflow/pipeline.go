// Copyright 2019 OKN Authors
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

package openflow

import (
	"fmt"
	"net"

	"okn/pkg/ovs/openflow"
)

const (
	// Flow table id index
	classifierTable       openflow.TableIDType = 0
	spoofGuardTable       openflow.TableIDType = 10
	arpResponderTable     openflow.TableIDType = 20
	conntrackTable        openflow.TableIDType = 30
	conntrackStateTable   openflow.TableIDType = 31
	dnatTable             openflow.TableIDType = 40
	egressRuleTable       openflow.TableIDType = 50
	egressDefaultTable    openflow.TableIDType = 60
	l3ForwardingTable     openflow.TableIDType = 70
	l2ForwardingCalcTable openflow.TableIDType = 80
	ingressRuleTable      openflow.TableIDType = 90
	ingressDefaultTable   openflow.TableIDType = 100
	l2ForwardingOutTable  openflow.TableIDType = 110

	// Flow priority level
	priorityMiss   = 80
	priorityNormal = 200

	// Traffic marks
	markTrafficFromTunnel  = 0
	markTrafficFromGateway = 1
	markTrafficFromLocal   = 2
)

type regType uint

func (rt regType) number() string {
	return fmt.Sprint(rt)
}

func (rt regType) nxm() string {
	return fmt.Sprintf("NXM_NX_REG%d", rt)
}

func (rt regType) reg() string {
	return fmt.Sprintf("reg%d", rt)
}

func i2h(data int64) string {
	return fmt.Sprintf("0x%x", data)
}

const (
	emptyPlaceholderStr = ""
	// marksReg stores traffic-source mark and pod-found mark.
	// traffic-source resides in [0..15], pod-found resides in [16..31]
	marksReg     regType = 0
	portCacheReg regType = 1

	ctZone = 0xfff0

	ctMarkField  = "ct_mark"
	ctStateFiled = "ct_state"
	inPortField  = "in_port"

	portFoundMark = 0x1
	gatewayCTMark = 0x20

	ipProtocol  = "ip"
	arpProtocol = "arp"

	globalVirtualMAC = "aa:bb:cc:dd:ee:ff"
)

type client struct {
	bridge                                    *openflow.Bridge
	pipeline                                  map[openflow.TableIDType]*openflow.Table
	nodeFlowCache, podFlowCache, serviceCache map[string][]openflow.Flow // cache for correspond deletions
}

// defaultFlows generates the default flows of all tables.
func (c *client) defaultFlows() (flows []openflow.Flow) {
	for _, table := range c.pipeline {
		flowBuilder := table.BuildFlow().Priority(priorityMiss).MatchProtocol(ipProtocol)
		switch table.MissAction {
		case openflow.TableMissActionNext:
			flowBuilder = flowBuilder.Action().Resubmit(emptyPlaceholderStr, table.Next)
		case openflow.TableMissActionNormal:
			flowBuilder = flowBuilder.Action().Normal()
		case openflow.TableMissActionDrop:
			fallthrough
		default:
			flowBuilder = flowBuilder.Action().Drop()
		}
		flows = append(flows, flowBuilder.Done())
	}
	return flows
}

// tunnelClassifierFlow generates the flow to mark traffic comes from the tunnelOFPort.
func (c *client) tunnelClassifierFlow(tunnelOFPort uint32) openflow.Flow {
	return c.pipeline[classifierTable].BuildFlow().Priority(priorityNormal).
		MatchField(inPortField, fmt.Sprint(tunnelOFPort)).
		Action().LoadRange(marksReg.reg(), markTrafficFromTunnel, openflow.Range{0, 15}).
		Action().Resubmit(emptyPlaceholderStr, conntrackStateTable).
		Done()
}

// gatewayClassifierFlow generates the flow to mark traffic comes from the gatewayOFPort.
func (c *client) gatewayClassifierFlow(gatewayOFPort uint32) openflow.Flow {
	classifierTable := c.pipeline[classifierTable]
	return classifierTable.BuildFlow().Priority(priorityNormal).
		MatchField(inPortField, fmt.Sprint(gatewayOFPort)).
		Action().LoadRange(marksReg.reg(), markTrafficFromGateway, openflow.Range{0, 15}).
		Action().Resubmit(emptyPlaceholderStr, classifierTable.Next).
		Done()
}

// podClassifierFlow generates the flow to mark traffic comes from the podOFPort.
func (c *client) podClassifierFlow(podOFPort uint32) openflow.Flow {
	classifierTable := c.pipeline[classifierTable]
	return classifierTable.BuildFlow().Priority(priorityNormal-10).
		MatchField(inPortField, fmt.Sprint(podOFPort)).
		Action().LoadRange(marksReg.reg(), markTrafficFromLocal, openflow.Range{0, 15}).
		Action().Resubmit(emptyPlaceholderStr, classifierTable.Next).
		Done()
}

// connectionTrackFlows generates flows that redirect traffic to ct_zone and handle traffic according to ct_state:
// 1) commit new connections to ct that sent from non-gateway.
// 2) Add ct_mark on traffic replied from the host gateway.
// 3) Cache src MAC if traffic comes from the host gateway and rewrite the dst MAC on traffic replied from Pod to the
// cached MAC.
// 4) Drop all invalid traffic.
func (c *client) connectionTrackFlows() (flows []openflow.Flow) {
	connectionTrackTable := c.pipeline[conntrackTable]
	baseConnectionTrackFlow := connectionTrackTable.BuildFlow().MatchProtocol(ipProtocol).Priority(priorityNormal).
		Action().CT(false, connectionTrackTable.Next, ctZone).
		Done()
	flows = append(flows, baseConnectionTrackFlow)

	connectionTrackStateTable := c.pipeline[conntrackStateTable]
	gatewayReplyFlow := connectionTrackStateTable.BuildFlow().MatchProtocol(ipProtocol).Priority(priorityNormal+10).
		MatchFieldRange(marksReg.reg(), fmt.Sprint(markTrafficFromGateway), openflow.Range{0, 15}).
		MatchField(ctMarkField, i2h(gatewayCTMark)).
		MatchField(ctStateFiled, "-new+trk").
		Action().Resubmit(emptyPlaceholderStr, connectionTrackStateTable.Next).
		Done()
	flows = append(flows, gatewayReplyFlow)

	gatewaySendFlow := connectionTrackStateTable.BuildFlow().MatchProtocol(ipProtocol).Priority(priorityNormal).
		MatchFieldRange(marksReg.reg(), fmt.Sprint(markTrafficFromGateway), openflow.Range{0, 15}).
		MatchField(ctStateFiled, "+new+trk").
		Action().
		CT(
			true,
			connectionTrackStateTable.Next,
			ctZone,
			fmt.Sprintf("load:0x%x->%s", gatewayCTMark, "NXM_NX_CT_MARK[]"),
			fmt.Sprintf("move:NXM_OF_ETH_SRC[]->NXM_NX_CT_LABEL[0..47]"),
		).
		Done()
	flows = append(flows, gatewaySendFlow)

	podReplyGatewayFlow := connectionTrackStateTable.BuildFlow().MatchProtocol(ipProtocol).Priority(priorityNormal).
		MatchField(ctMarkField, i2h(gatewayCTMark)).
		MatchField(ctStateFiled, "-new+trk").
		Action().MoveRange("NXM_NX_CT_LABEL", "NXM_OF_ETH_DST", openflow.Range{0, 47}, openflow.Range{0, 47}).
		Action().Resubmit(emptyPlaceholderStr, connectionTrackStateTable.Next).
		Done()
	flows = append(flows, podReplyGatewayFlow)

	nonGatewaySendFlow := connectionTrackStateTable.BuildFlow().MatchProtocol(ipProtocol).Priority(priorityNormal-10).
		MatchField(ctStateFiled, "+new+trk").
		Action().CT(true, connectionTrackStateTable.Next, ctZone).
		Done()
	flows = append(flows, nonGatewaySendFlow)

	invCTFlow := connectionTrackStateTable.BuildFlow().MatchProtocol(ipProtocol).Priority(priorityNormal).
		MatchField(ctStateFiled, "+new+inv").
		Action().Drop().
		Done()
	flows = append(flows, invCTFlow)

	return flows
}

// l2ForwardCalcFlow generates the flow that matches dst MAC and loads ofPort to reg.
func (c *client) l2ForwardCalcFlow(dstMAC string, ofPort uint32) openflow.Flow {
	l2FwdCalcTable := c.pipeline[l2ForwardingCalcTable]
	return l2FwdCalcTable.BuildFlow().Priority(priorityNormal).
		MatchField("dl_dst", dstMAC).
		Action().LoadRange(portCacheReg.nxm(), ofPort, openflow.Range{0, 31}).
		Action().LoadRange(marksReg.nxm(), portFoundMark, openflow.Range{16, 31}).
		Action().Resubmit(emptyPlaceholderStr, l2FwdCalcTable.Next).
		Done()
}

// l2ForwardOutputFlow generates the flow that outputs packets to OVS port after L2 forwarding calculation.
func (c *client) l2ForwardOutputFlow() openflow.Flow {
	return c.pipeline[l2ForwardingOutTable].BuildFlow().
		Priority(priorityNormal).
		MatchProtocol(ipProtocol).
		MatchFieldRange(marksReg.reg(), i2h(portFoundMark), openflow.Range{16, 31}).
		Action().OutputFieldRange(portCacheReg.nxm(), openflow.Range{0, 31}).
		Done()
}

// l3FlowsToPod generates the flow to rewrite MAC if the packet is received from tunnel port and destined for local Pods.
func (c *client) l3FlowsToPod(localGatewayMAC string, podInterfaceIP string, podInterfaceMAC string) openflow.Flow {
	l3FwdTable := c.pipeline[l3ForwardingTable]
	// Rewrite src MAC to local gateway MAC, and rewrite dst MAC to pod MAC
	return l3FwdTable.BuildFlow().MatchProtocol(ipProtocol).Priority(priorityNormal).
		MatchField("dl_dst", globalVirtualMAC).
		MatchField("nw_dst", podInterfaceIP).
		Action().SetField("dl_src", localGatewayMAC).
		Action().SetField("dl_dst", podInterfaceMAC).
		Action().DecTTL().
		Action().Resubmit(emptyPlaceholderStr, l3FwdTable.Next).
		Done()
}

// l3ToGatewayFlow generates flow that rewrites MAC of the packet received from tunnel port and destined to local gateway.
func (c *client) l3ToGatewayFlow(localGatewayIP string, localGatewayMAC string) openflow.Flow {
	l3FwdTable := c.pipeline[l3ForwardingTable]
	return l3FwdTable.BuildFlow().MatchProtocol(ipProtocol).Priority(priorityNormal).
		MatchField("nw_dst", localGatewayIP).
		Action().SetField("dl_dst", localGatewayMAC).
		Action().Resubmit(emptyPlaceholderStr, l3FwdTable.Next).
		Done()
}

// l3FwdFlowToRemote generates the L3 forward flow on source node to support traffic to remote pods/gateway.
func (c *client) l3FwdFlowToRemote(localGatewayMAC, peerSubnet, peerTunnel string) openflow.Flow {
	l3FwdTable := c.pipeline[l3ForwardingTable]
	// Rewrite src MAC to local gateway MAC and rewrite dst MAC to virtual MAC
	return l3FwdTable.BuildFlow().MatchProtocol(ipProtocol).Priority(priorityNormal).
		MatchField("nw_dst", peerSubnet).
		Action().DecTTL().
		Action().SetField("dl_src", localGatewayMAC).
		Action().SetField("dl_dst", globalVirtualMAC).
		Action().SetField("tun_dst", peerTunnel).
		Action().Resubmit(emptyPlaceholderStr, l3FwdTable.Next).
		Done()
}

// arpResponderFlow generates the ARP responder flow entry that replies request comes from local gateway for peer
// gateway MAC.
func (c *client) arpResponderFlow(peerGatewayIP string) openflow.Flow {
	return c.pipeline[arpResponderTable].BuildFlow().
		MatchProtocol(arpProtocol).Priority(priorityNormal).
		MatchField("arp_op", "1").
		MatchField("arp_tpa", peerGatewayIP).
		Action().Move("NXM_OF_ETH_SRC", "NXM_OF_ETH_DST").
		Action().SetField("dl_src", globalVirtualMAC).
		Action().Load("NXM_OF_ARP_OP", 2).
		Action().Move("NXM_NX_ARP_SHA", "NXM_NX_ARP_THA").
		Action().SetField("arp_sha", globalVirtualMAC).
		Action().Move("NXM_OF_ARP_SPA", "NXM_OF_ARP_TPA").
		Action().SetField("arp_spa", peerGatewayIP).
		Action().OutputInPort().
		Done()
}

// podIPSpoofGuardFlow generates the flow to check IP traffic sent out from local pod. Traffic from host gateway interface
// will not be checked, since it might be pod to service traffic or host namespace traffic.
func (c *client) podIPSpoofGuardFlow(ifIP string, ifMAC string, ifOfPort uint32) openflow.Flow {
	ipPipeline := c.pipeline
	ipSpoofGuardTable := ipPipeline[spoofGuardTable]
	return ipSpoofGuardTable.BuildFlow().MatchProtocol(ipProtocol).Priority(priorityNormal).
		MatchField("in_port", fmt.Sprint(ifOfPort)).
		MatchField("dl_src", ifMAC).
		MatchField("nw_src", ifIP).
		Action().Resubmit(emptyPlaceholderStr, ipSpoofGuardTable.Next).
		Done()
}

// gatewayARPSpoofGuardFlow generates the flow to skip ARP UP check on packets sent out from the local gateway interface.
func (c *client) gatewayARPSpoofGuardFlow(gatewayOFPort uint32) openflow.Flow {
	return c.pipeline[spoofGuardTable].BuildFlow().MatchProtocol(arpProtocol).Priority(priorityNormal).
		MatchField("in_port", fmt.Sprint(gatewayOFPort)).
		Action().Resubmit(emptyPlaceholderStr, arpResponderTable).
		Done()
}

// arpSpoofGuardFlow generates the flow to check ARP traffic sent out from local pods interfaces.
func (c *client) arpSpoofGuardFlow(ifIP string, ifMAC string, ifOFPort uint32) openflow.Flow {
	return c.pipeline[spoofGuardTable].BuildFlow().MatchProtocol(arpProtocol).Priority(priorityNormal).
		MatchField("in_port", fmt.Sprint(ifOFPort)).
		MatchField("arp_sha", ifMAC).
		MatchField("arp_spa", ifIP).
		Action().Resubmit(emptyPlaceholderStr, arpResponderTable).
		Done()
}

// gatewayIPSpoofGuardFlow generates the flow to skip spoof guard checking for traffic sent from gateway interface.
func (c *client) gatewayIPSpoofGuardFlow(gatewayOFPort uint32) openflow.Flow {
	ipPipeline := c.pipeline
	ipSpoofGuardTable := ipPipeline[spoofGuardTable]
	return ipSpoofGuardTable.BuildFlow().Priority(priorityNormal).
		MatchProtocol(ipProtocol).
		MatchField("in_port", fmt.Sprint(gatewayOFPort)).
		Action().Resubmit(emptyPlaceholderStr, ipSpoofGuardTable.Next).
		Done()
}

// serviceCIDRDNATFlow generates flows to match dst IP in service CIDR and output to host gateway interface directly.
func (c *client) serviceCIDRDNATFlow(serviceCIDR *net.IPNet, gatewayOFPort uint32) openflow.Flow {
	return c.pipeline[dnatTable].BuildFlow().MatchProtocol(ipProtocol).Priority(priorityNormal).
		MatchField("nw_dst", serviceCIDR.String()).
		Action().Output(int(gatewayOFPort)).
		Done()
}

// arpNormalFlow generates the flow to response arp in normal way if no flow in arpResponderTable is matched.
func (c *client) arpNormalFlow() openflow.Flow {
	return c.pipeline[arpResponderTable].BuildFlow().
		MatchProtocol(arpProtocol).Priority(priorityNormal - 10).
		Action().Normal().Done()
}

// NewClient is the constructor of the Client interface.
func NewClient(bridgeName string) Client {
	bridge := &openflow.Bridge{Name: bridgeName}
	c := &client{
		bridge: bridge,
		pipeline: map[openflow.TableIDType]*openflow.Table{
			classifierTable:       bridge.CreateTable(classifierTable, spoofGuardTable, openflow.TableMissActionNext),
			spoofGuardTable:       bridge.CreateTable(spoofGuardTable, conntrackTable, openflow.TableMissActionDrop),
			conntrackTable:        bridge.CreateTable(conntrackTable, conntrackStateTable, openflow.TableMissActionNext),
			conntrackStateTable:   bridge.CreateTable(conntrackStateTable, dnatTable, openflow.TableMissActionNext),
			dnatTable:             bridge.CreateTable(dnatTable, l3ForwardingTable, openflow.TableMissActionNext),
			l3ForwardingTable:     bridge.CreateTable(l3ForwardingTable, l2ForwardingCalcTable, openflow.TableMissActionNext),
			l2ForwardingCalcTable: bridge.CreateTable(l2ForwardingCalcTable, l2ForwardingOutTable, openflow.TableMissActionNext),
			l2ForwardingOutTable:  bridge.CreateTable(l2ForwardingOutTable, openflow.LastTableID, openflow.TableMissActionDrop),
			arpResponderTable:     bridge.CreateTable(arpResponderTable, openflow.LastTableID, openflow.TableMissActionDrop),
		},
		nodeFlowCache: map[string][]openflow.Flow{},
		podFlowCache:  map[string][]openflow.Flow{},
		serviceCache:  map[string][]openflow.Flow{},
	}
	return c
}