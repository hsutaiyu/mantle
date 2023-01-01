package eth

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/mantlenetworkio/mantle/l2geth/core"
	"github.com/mantlenetworkio/mantle/l2geth/core/types"
	"github.com/mantlenetworkio/mantle/l2geth/log"
	"github.com/mantlenetworkio/mantle/l2geth/p2p"
	"github.com/mantlenetworkio/mantle/l2geth/p2p/enode"
)

func (pm *ProtocolManager) makeConsensusProtocol(version uint) p2p.Protocol {
	length := consensusProtocolLength

	return p2p.Protocol{
		Name:    consensusProtocolName,
		Version: version,
		Length:  length,
		Run:     pm.consensusHandler,
		NodeInfo: func() interface{} {
			return pm.NodeInfo()
		},
		PeerInfo: func(id enode.ID) interface{} {
			if p := pm.peers.Peer(fmt.Sprintf("%x", id[:8])); p != nil {
				return p.Info()
			}
			return nil
		},
	}
}

func (pm *ProtocolManager) removePeerTmp(id string) {
	// Short circuit if the peer was already removed
	peer := pm.consensusPeers.Peer(id)
	if peer == nil {
		return
	}
	log.Debug("Removing Ethereum consensus peer", "peer", id)

	if err := pm.consensusPeers.Unregister(id); err != nil {
		log.Error("Consensus Peer removal failed", "peer", id, "err", err)
	}
	// Hard disconnect at the networking layer
	if peer != nil {
		peer.Peer.Disconnect(p2p.DiscUselessPeer)
	}
}

func (pm *ProtocolManager) consensusHandler(peer *p2p.Peer, rw p2p.MsgReadWriter) error {
	p := pm.newPeer(int(eth64), peer, rw)
	select {
	case pm.newPeerCh <- p:
		pm.wg.Add(1)
		defer pm.wg.Done()
		// Ignore maxPeers if this is a trusted peer
		if pm.consensusPeers.Len() >= pm.maxPeers && !p.Peer.Info().Network.Trusted {
			return p2p.DiscTooManyPeers
		}
		p.Log().Debug("Ethereum consensus peer connected", "name", p.Name())
		// Execute the Ethereum handshake
		//var (
		//	genesis = pm.blockchain.Genesis()
		//	head    = pm.blockchain.CurrentHeader()
		//	hash    = head.Hash()
		//	number  = head.Number.Uint64()
		//	td      = pm.blockchain.GetTd(hash, number)
		//)
		//if err := p.Handshake(pm.networkID, td, hash, genesis.Hash(), forkid.NewID(pm.blockchain), pm.forkFilter); err != nil {
		//	p.Log().Debug("Ethereum handshake failed", "err", err)
		//	return err
		//}
		if rw, ok := p.rw.(*meteredMsgReadWriter); ok {
			rw.Init(p.version)
		}
		// Register the peer locally
		if err := pm.consensusPeers.Register(p); err != nil {
			p.Log().Error("Ethereum peer registration failed", "err", err)
			return err
		}
		defer pm.removePeerTmp(p.id)

		// Handle incoming messages until the connection is torn down
		for {
			if pm.minerCheck() {
				if err := pm.checkPeer(p); err != nil {
					return err
				}
			}

			if err := pm.handleConsensusMsg(p); err != nil {
				fmt.Println("pm.networkID:", pm.networkID)
				fmt.Println("pm.acceptTxs:", pm.acceptTxs)
				p.Log().Debug("Ethereum consensus message handling failed", "err", err)
				return err
			}
		}
	case <-pm.quitSync:
		return p2p.DiscQuitting
	}
}

func (pm *ProtocolManager) checkPeer(p *peer) error {
	if bytes.Equal(pm.etherbase.Bytes(), pm.schedulerInst.Scheduler().Bytes()) {
		has := make(chan bool)
		if err := pm.eventMux.Post(core.PeerAddEvent{
			PeerId: p.ID().Bytes(),
			Has:    has,
		}); err != nil {
			return err
		}

		p.Log().Debug("wait for peer id check", "ID", p.ID().String())
		select {
		case find := <-has:
			if !find {
				p.Log().Debug("Have not find peer ", "ID", p.ID().String())
				return errors.New("have not find peer")
			} else {
				p.Log().Debug("find peer ", "ID", p.ID().String())
			}
		}
	}
	return nil
}

func (pm *ProtocolManager) handleConsensusMsg(p *peer) error {
	// Read the next message from the remote peer, and ensure it's fully consumed
	msg, err := p.rw.ReadMsg()
	if err != nil {
		return err
	}
	if msg.Size > consensusMaxMsgSize {
		return errResp(ErrMsgTooLarge, "%v > %v", msg.Size, protocolMaxMsgSize)
	}
	defer msg.Discard()
	fmt.Println("======msg.Code:", msg.Code)
	// Handle the message depending on its contents
	switch {
	case msg.Code == BatchPeriodStartMsg:
		var bs *types.BatchPeriodStartMsg
		if err := msg.Decode(&bs); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		log.Info("Batch Period Start Msg", "batchIndex", bs.BatchIndex, "startHeight", bs.StartHeight, "maxHeight", bs.MaxHeight, "expireTime", bs.ExpireTime)
		if !types.VerifySigner(bs, pm.schedulerInst.Scheduler()) {
			return nil
		}

		p.knowBatchPeriodStartMsg.Add(bs.Hash())
		erCh := make(chan error, 1)
		pm.eventMux.Post(core.BatchPeriodStartEvent{
			Msg:   bs,
			ErrCh: erCh,
		})
	case msg.Code == BatchPeriodAnswerMsg:
		var bpa *types.BatchPeriodAnswerMsg
		if err := msg.Decode(&bpa); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		log.Info("Batch Period Answer Msg", "batchIndex", bpa.BatchIndex, "StartIndex", bpa.StartIndex, "tx len", len(bpa.Txs))
		if !pm.schedulerInst.IsRunning() {
			log.Debug("not scheduler")
			return nil
		}
		if !types.VerifySigner(bpa, pm.schedulerInst.CurrentStartMsg().Sequencer) {
			return nil
		}
		p.knowBatchPeriodAnswerMsg.Add(bpa.Hash())
		erCh := make(chan error, 1)
		pm.eventMux.Post(core.BatchPeriodAnswerEvent{
			Msg:   bpa,
			ErrCh: erCh,
		})
	case msg.Code == FraudProofReorgMsg:
		var fpr *types.FraudProofReorgMsg
		if err := msg.Decode(&fpr); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		log.Info("Fraud Proof Reorg Msg")
		p.knowFraudProofReorg.Add(fpr.Hash())
		// todo: FraudProofReorgMsg handle

	default:
		return errResp(ErrInvalidMsgCode, "%v", msg.Code)
	}

	return nil
}

// ---------------------------- Consensus Control Messages ----------------------------

// BatchPeriodStartMsg will
func (pm *ProtocolManager) batchPeriodStartMsgBroadcastLoop() {
	log.Info("Start batchPeriodStartMsg broadcast routine")
	// automatically stops if unsubscribe
	for obj := range pm.batchStartMsgSub.Chan() {
		if se, ok := obj.Data.(core.BatchPeriodStartEvent); ok {
			log.Debug("Got BatchPeriodStartEvent, broadcast it",
				"batchIndex", se.Msg.BatchIndex,
				"startHeight", se.Msg.StartHeight,
				"maxHeight", se.Msg.MaxHeight)
			pm.BroadcastBatchPeriodStartMsg(se.Msg) // First propagate block to peers
		}
	}
}

func (pm *ProtocolManager) BroadcastBatchPeriodStartMsg(msg *types.BatchPeriodStartMsg) {
	peers := pm.consensusPeers.PeersWithoutStartMsg(msg.Hash())
	for _, p := range peers {
		p.AsyncSendBatchPeriodStartMsg(msg)
	}
	log.Trace("Broadcast batch period start msg")
}

func (p *peer) AsyncSendBatchPeriodStartMsg(msg *types.BatchPeriodStartMsg) {
	select {
	case p.queuedBatchStartMsg <- msg:
		p.knowBatchPeriodStartMsg.Add(msg.Hash())
		for p.knowBatchPeriodStartMsg.Cardinality() >= maxKnownStartMsg {
			p.knowBatchPeriodStartMsg.Pop()
		}

	default:
		p.Log().Debug("Dropping batch period start msg propagation", "batch index", msg.BatchIndex)
	}
}

// BatchPeriodAnswerMsg
func (pm *ProtocolManager) batchPeriodAnswerMsgBroadcastLoop() {
	log.Info("Start batchPeriodAnswerMsg broadcast routine")
	for obj := range pm.batchAnswerMsgSub.Chan() {
		if ee, ok := obj.Data.(core.BatchPeriodAnswerEvent); ok {
			log.Info("Broadcast BatchPeriodAnswerEvent", "Sequencer", ee.Msg.Sequencer, "tx_count", len(ee.Msg.Txs))
			pm.BroadcastBatchPeriodAnswerMsg(ee.Msg) // First propagate block to peers
		}
	}
}

func (pm *ProtocolManager) BroadcastBatchPeriodAnswerMsg(msg *types.BatchPeriodAnswerMsg) {
	peers := pm.consensusPeers.PeersWithoutEndMsg(msg.Hash())
	for _, p := range peers {
		p.AsyncSendBatchPeriodAnswerMsg(msg)
	}
	log.Trace("Broadcast batch period answer msg")
}

func (p *peer) AsyncSendBatchPeriodAnswerMsg(msg *types.BatchPeriodAnswerMsg) {
	select {
	case p.queuedBatchAnswerMsg <- msg:
		p.knowBatchPeriodAnswerMsg.Add(msg.Hash())
		for p.knowBatchPeriodAnswerMsg.Cardinality() >= maxKnownEndMsg {
			p.knowBatchPeriodAnswerMsg.Pop()
		}

	default:
		p.Log().Debug("Dropping batch period end msg propagation", "start index", msg.StartIndex)
	}
}

// FraudProofReorgMsg
func (pm *ProtocolManager) fraudProofReorgMsgBroadcastLoop() {
	log.Info("Start fraudProofReorgMsg broadcast routine")
	// automatically stops if unsubscribe
	for obj := range pm.fraudProofReorgMsgSub.Chan() {
		if fe, ok := obj.Data.(core.FraudProofReorgEvent); ok {
			log.Debug("Got BatchPeriodAnswerEvent, broadcast it",
				"reorgIndex", fe.Msg.ReorgIndex,
				"reorgToHeight", fe.Msg.ReorgToHeight)

			pm.BroadcastFraudProofReorgMsg(fe.Msg) // First propagate block to peers
		}
	}
}

func (pm *ProtocolManager) BroadcastFraudProofReorgMsg(reorg *types.FraudProofReorgMsg) {
	peers := pm.consensusPeers.PeersWithoutFraudProofReorgMsg(reorg.Hash())
	for _, p := range peers {
		p.AsyncSendFraudProofReorgMsg(reorg)
	}
	log.Trace("Broadcast fraud proof reorg msg")
}

func (p *peer) AsyncSendFraudProofReorgMsg(reorg *types.FraudProofReorgMsg) {
	select {
	case p.queuedFraudProofReorg <- reorg:
		p.knowFraudProofReorg.Add(reorg.Hash())
		for p.knowFraudProofReorg.Cardinality() >= maxKnownFraudProofReorgMsg {
			p.knowFraudProofReorg.Pop()
		}

	default:
		p.Log().Debug("Dropping producers propagation", "reorg index", reorg.ReorgIndex)
	}
}

// ---------------------------- Proposers ----------------------------

// SendBatchPeriodStart sends a batch of transaction receipts, corresponding to the
// ones requested from an already RLP encoded format.
func (p *peer) SendBatchPeriodStart(bps *types.BatchPeriodStartMsg) error {
	p.knowBatchPeriodStartMsg.Add(bps.Hash())
	// Mark all the producers as known, but ensure we don't overflow our limits
	for p.knowBatchPeriodStartMsg.Cardinality() >= maxKnownStartMsg {
		p.knowBatchPeriodStartMsg.Pop()
	}
	return p2p.Send(p.rw, BatchPeriodStartMsg, bps)
}

func (p *peer) SendBatchPeriodAnswer(bpa *types.BatchPeriodAnswerMsg) error {
	p.knowBatchPeriodAnswerMsg.Add(bpa.Hash())
	// Mark all the producers as known, but ensure we don't overflow our limits
	for p.knowBatchPeriodAnswerMsg.Cardinality() >= maxKnownEndMsg {
		p.knowBatchPeriodAnswerMsg.Pop()
	}
	return p2p.Send(p.rw, BatchPeriodAnswerMsg, bpa)
}

func (p *peer) SendFraudProofReorg(fpr *types.FraudProofReorgMsg) error {
	p.knowFraudProofReorg.Add(fpr.Hash())
	// Mark all the producers as known, but ensure we don't overflow our limits
	for p.knowFraudProofReorg.Cardinality() >= maxKnownFraudProofReorgMsg {
		p.knowFraudProofReorg.Pop()
	}
	return p2p.Send(p.rw, FraudProofReorgMsg, fpr)
}
