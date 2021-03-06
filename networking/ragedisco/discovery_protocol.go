package ragedisco

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/multierr"

	"github.com/protofire/celo-libocr/commontypes"
	nettypes "github.com/protofire/celo-libocr/networking/types"
	ragetypes "github.com/protofire/celo-libocr/ragep2p/types"

	"github.com/pkg/errors"
	"github.com/protofire/celo-libocr/internal/loghelper"
	"github.com/protofire/celo-libocr/offchainreporting2/types"
	"github.com/protofire/celo-libocr/subprocesses"
)

type incomingMessage struct {
	payload WrappableMessage
	from    ragetypes.PeerID
}

type outgoingMessage struct {
	payload WrappableMessage
	to      ragetypes.PeerID
}

type discoveryProtocolState int

const (
	_ discoveryProtocolState = iota
	discoveryProtocolUnstarted
	discoveryProtocolStarted
	discoveryProtocolClosed
)

// discoveryProtocolLocked contains a subset of a discoveryProtocol's state
// that requires the discoveryProtocol lock to be held in order to access or
// modify
type discoveryProtocolLocked struct {
	bestAnnouncement        map[ragetypes.PeerID]Announcement
	groups                  map[types.ConfigDigest]*group
	bootstrappers           map[ragetypes.PeerID]map[ragetypes.Address]int
	numGroupsByOracle       map[ragetypes.PeerID]int
	numGroupsByBootstrapper map[ragetypes.PeerID]int
}

type discoveryProtocol struct {
	stateMu sync.Mutex
	state   discoveryProtocolState

	deltaReconcile     time.Duration
	chIncomingMessages <-chan incomingMessage
	chOutgoingMessages chan<- outgoingMessage
	chConnectivity     chan<- connectivityMsg
	chInternalBump     chan Announcement
	privKey            ed25519.PrivateKey
	ownID              ragetypes.PeerID
	ownAddrs           []ragetypes.Address

	lock   sync.RWMutex
	locked discoveryProtocolLocked

	db nettypes.DiscovererDatabase

	processes subprocesses.Subprocesses
	ctx       context.Context
	ctxCancel context.CancelFunc
	logger    loghelper.LoggerWithContext
}

const (
	announcementVersionWarnThreshold = 100e6

	saveInterval       = 2 * time.Minute
	reportInitialDelay = 10 * time.Second
	reportInterval     = 5 * time.Minute
)

func newDiscoveryProtocol(
	deltaReconcile time.Duration,
	chIncomingMessages <-chan incomingMessage,
	chOutgoingMessages chan<- outgoingMessage,
	chConnectivity chan<- connectivityMsg,
	privKey ed25519.PrivateKey,
	ownAddrs []ragetypes.Address,
	db nettypes.DiscovererDatabase,
	logger loghelper.LoggerWithContext,
) (*discoveryProtocol, error) {
	ownID, err := ragetypes.PeerIDFromPrivateKey(privKey)
	if err != nil {
		return nil, errors.Wrap(err, "failed to obtain peer id from private key")
	}

	ctx, ctxCancel := context.WithCancel(context.Background())
	return &discoveryProtocol{
		sync.Mutex{},
		discoveryProtocolUnstarted,
		deltaReconcile,
		chIncomingMessages,
		chOutgoingMessages,
		chConnectivity,
		make(chan Announcement),
		privKey,
		ownID,
		ownAddrs,
		sync.RWMutex{},
		discoveryProtocolLocked{
			make(map[ragetypes.PeerID]Announcement),
			make(map[types.ConfigDigest]*group),
			make(map[ragetypes.PeerID]map[ragetypes.Address]int),
			make(map[ragetypes.PeerID]int),
			make(map[ragetypes.PeerID]int),
		},
		db,
		subprocesses.Subprocesses{},
		ctx,
		ctxCancel,
		logger.MakeChild(commontypes.LogFields{"struct": "discoveryProtocol"}),
	}, nil
}

func (p *discoveryProtocol) Start() error {
	succeeded := false
	defer func() {
		if !succeeded {
			p.Close()
		}
	}()

	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if p.state != discoveryProtocolUnstarted {
		return fmt.Errorf("cannot start discoveryProtocol that is not unstarted, state was: %v", p.state)
	}
	p.state = discoveryProtocolStarted

	p.lock.Lock()
	defer p.lock.Unlock()
	_, _, err := p.lockedBumpOwnAnnouncement()
	if err != nil {
		return errors.Wrap(err, "failed to bump own announcement")
	}
	p.processes.Go(p.recvLoop)
	p.processes.Go(p.sendLoop)
	p.processes.Go(p.saveLoop)
	p.processes.Go(p.statusReportLoop)
	succeeded = true
	return nil
}

func formatAnnouncementsForReport(allIDs map[ragetypes.PeerID]struct{}, baSigned map[ragetypes.PeerID]Announcement) (string, int) {
	// Would use json here but I want to avoid having quotes in logs as it would cause escaping all over the place.
	var sb strings.Builder
	sb.WriteRune('{')
	i := 0
	undetected := 0
	for id := range allIDs {
		ann, exists := baSigned[id]
		var s string
		if exists {
			s = ann.unsignedAnnouncement.String()
		} else {
			s = "<not found>"
			undetected++
		}

		if i != 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(id.String())
		sb.WriteString(": ")
		sb.WriteString(s)
		i++
	}
	sb.WriteRune('}')
	return sb.String(), undetected
}

func (p *discoveryProtocol) statusReportLoop() {
	chDone := p.ctx.Done()
	timer := time.After(reportInitialDelay)
	for {
		select {
		case <-timer:
			func() {
				p.lock.RLock()
				defer p.lock.RUnlock()
				uniquePeersToDetect := make(map[ragetypes.PeerID]struct{})
				for id, cnt := range p.locked.numGroupsByOracle {
					if cnt == 0 {
						continue
					}
					uniquePeersToDetect[id] = struct{}{}
				}

				reportStr, undetected := formatAnnouncementsForReport(uniquePeersToDetect, p.locked.bestAnnouncement)
				p.logger.Info("Discoverer status report", commontypes.LogFields{
					"statusByPeer":    reportStr,
					"peersToDetect":   len(uniquePeersToDetect),
					"peersUndetected": undetected,
					"peersDetected":   len(uniquePeersToDetect) - undetected,
				})
				timer = time.After(reportInterval)
			}()
		case <-chDone:
			return
		}
	}
}

// Peer A is allowed to learn about an Announcement by peer B if B is an oracle node in
// one of the groups A participates in.
func (p *discoveryProtocol) lockedAllowedPeers(ann Announcement) (ps []ragetypes.PeerID) {
	annPeerID, err := ann.PeerID()
	if err != nil {
		p.logger.Warn("failed to obtain peer id from announcement", reason(err))
		return
	}
	peers := make(map[ragetypes.PeerID]struct{})
	for _, g := range p.locked.groups {
		if !g.hasOracle(annPeerID) {
			continue
		}
		for _, pid := range g.peerIDs() {
			peers[pid] = struct{}{}
		}
	}
	for pid := range peers {
		if pid == p.ownID {
			continue
		}
		ps = append(ps, pid)
	}
	return
}

func (p *discoveryProtocol) addGroup(digest types.ConfigDigest, onodes []ragetypes.PeerID, bnodes []ragetypes.PeerInfo) error {
	var newPeerIDs []ragetypes.PeerID
	p.lock.Lock()
	defer p.lock.Unlock()

	if _, exists := p.locked.groups[digest]; exists {
		return fmt.Errorf("asked to add group with digest we already have (digest: %s)", digest.Hex())
	}
	newGroup := group{oracleNodes: onodes, bootstrapperNodes: bnodes}
	p.locked.groups[digest] = &newGroup
	for _, oid := range onodes {
		if p.locked.numGroupsByOracle[oid] == 0 {
			newPeerIDs = append(newPeerIDs, oid)
		}
		p.locked.numGroupsByOracle[oid]++
	}
	for _, bs := range bnodes {
		p.locked.numGroupsByBootstrapper[bs.ID]++
		for _, addr := range bs.Addrs {
			if _, exists := p.locked.bootstrappers[bs.ID]; !exists {
				p.locked.bootstrappers[bs.ID] = make(map[ragetypes.Address]int)
			}
			p.locked.bootstrappers[bs.ID][addr]++
		}
	}
	for _, pid := range newGroup.peerIDs() {
		// it's ok to send connectivityAdd messages multiple times
		select {
		case p.chConnectivity <- connectivityMsg{connectivityAdd, pid}:
		case <-p.ctx.Done():
			return nil
		}
	}

	// we hold lock here
	if err := p.lockedLoadFromDB(newPeerIDs); err != nil {
		// db-level errors are not prohibitive
		p.logger.Warn("Failed to load announcements from db", reason(err))
	}
	return nil
}

func (p *discoveryProtocol) lockedLoadFromDB(ragePeerIDs []ragetypes.PeerID) error {
	// The database may have been set to nil, and we don't necessarily need it to function.
	if len(ragePeerIDs) == 0 || p.db == nil {
		return nil
	}
	strPeerIDs := make([]string, len(ragePeerIDs))
	for i, pid := range ragePeerIDs {
		strPeerIDs[i] = pid.String()
	}
	annByID, err := p.db.ReadAnnouncements(p.ctx, strPeerIDs)
	if err != nil {
		return err
	}
	for _, dbannBytes := range annByID {
		dbann, err := deserializeSignedAnnouncement(dbannBytes)
		if err != nil {
			p.logger.Warn("failed to deserialize signed announcement from db", commontypes.LogFields{
				"error": err,
				"bytes": dbannBytes,
			})
			continue
		}
		err = p.lockedProcessAnnouncement(dbann)
		if err != nil {
			p.logger.Warn("failed to process announcement from db", commontypes.LogFields{
				"error": err,
				"ann":   dbann,
			})
		}
	}
	return nil
}

func (p *discoveryProtocol) saveAnnouncementToDB(ann Announcement) error {
	if p.db == nil {
		return nil
	}
	ser, err := ann.serialize()
	if err != nil {
		return err
	}
	pid, err := ann.PeerID()
	if err != nil {
		return err
	}
	return p.db.StoreAnnouncement(p.ctx, pid.String(), ser)
}

func (p *discoveryProtocol) saveToDB() error {
	if p.db == nil {
		return nil
	}
	p.lock.RLock()
	defer p.lock.RUnlock()

	var allErrors error
	for _, ann := range p.locked.bestAnnouncement {
		allErrors = multierr.Append(allErrors, p.saveAnnouncementToDB(ann))
	}
	return allErrors
}

func (p *discoveryProtocol) saveLoop() {
	if p.db == nil {
		return
	}
	for {
		select {
		case <-time.After(saveInterval):
		case <-p.ctx.Done():
			return
		}

		if err := p.saveToDB(); err != nil {
			p.logger.Warn("failed to save announcements to db", reason(err))
		}
	}
}

func (p *discoveryProtocol) removeGroup(digest types.ConfigDigest) error {
	logger := p.logger.MakeChild(commontypes.LogFields{"in": "removeGroup"})
	logger.Trace("Called", nil)
	p.lock.Lock()
	defer p.lock.Unlock()

	goneGroup, exists := p.locked.groups[digest]
	if !exists {
		return fmt.Errorf("can't remove group that is not registered (digest: %s)", digest.Hex())
	}

	delete(p.locked.groups, digest)

	for _, oid := range goneGroup.oracleIDs() {
		p.locked.numGroupsByOracle[oid]--
		if p.locked.numGroupsByOracle[oid] == 0 {
			if ann, exists := p.locked.bestAnnouncement[oid]; exists {
				if err := p.saveAnnouncementToDB(ann); err != nil {
					p.logger.Warn("Failed to save announcement from removed group to DB", reason(err))
				}
			}
			if oid != p.ownID {
				delete(p.locked.bestAnnouncement, oid)
			}
			delete(p.locked.numGroupsByOracle, oid)
		}
	}

	for _, binfo := range goneGroup.bootstrapperNodes {
		bid := binfo.ID

		p.locked.numGroupsByBootstrapper[bid]--
		if p.locked.numGroupsByBootstrapper[bid] == 0 {
			delete(p.locked.numGroupsByBootstrapper, bid)
			delete(p.locked.bootstrappers, bid)
			continue
		}
		for _, addr := range binfo.Addrs {
			p.locked.bootstrappers[bid][addr]--
			if p.locked.bootstrappers[bid][addr] == 0 {
				delete(p.locked.bootstrappers[bid], addr)
			}
		}
	}

	// Cleanup connections for peers we don't have in any group anymore.
	for _, pid := range goneGroup.peerIDs() {
		if p.locked.numGroupsByOracle[pid]+p.locked.numGroupsByBootstrapper[pid] == 0 {
			select {
			case p.chConnectivity <- connectivityMsg{connectivityRemove, pid}:
			case <-p.ctx.Done():
				return nil
			}
		}
	}

	return nil
}

func (p *discoveryProtocol) FindPeer(peer ragetypes.PeerID) (addrs []ragetypes.Address, err error) {
	allAddrs := make(map[ragetypes.Address]struct{})
	p.lock.RLock()
	defer p.lock.RUnlock()
	if baddrs, ok := p.locked.bootstrappers[peer]; ok {
		for a := range baddrs {
			allAddrs[a] = struct{}{}
		}
	}
	if ann, ok := p.locked.bestAnnouncement[peer]; ok {
		for _, a := range ann.Addrs {
			allAddrs[a] = struct{}{}
		}
	}
	for a := range allAddrs {
		addrs = append(addrs, a)
	}
	return
}

func (p *discoveryProtocol) recvLoop() {
	logger := p.logger.MakeChild(commontypes.LogFields{"in": "recvLoop"})
	logger.Debug("Entering", nil)
	defer logger.Debug("Exiting", nil)
	for {
		select {
		case <-p.ctx.Done():
			return
		case msg := <-p.chIncomingMessages:
			logger := logger.MakeChild(commontypes.LogFields{"remotePeerID": msg.from})
			switch v := msg.payload.(type) {
			case *Announcement:
				logger.Trace("Received announcement", v.toLogFields())
				if err := p.processAnnouncement(*v); err != nil {
					logger := logger.MakeChild(reason(err))
					logger = logger.MakeChild(v.toLogFields())
					logger.Warn("Failed to process announcement", nil)
				}
			case *reconcile:
				// logger.Trace("Received reconcile", commontypes.LogFields{"reconcile": v.toLogFields()})
				for _, ann := range v.Anns {
					if err := p.processAnnouncement(ann); err != nil {
						logger := logger.MakeChild(reason(err))
						logger = logger.MakeChild(v.toLogFields())
						logger = logger.MakeChild(ann.toLogFields())
						logger.Warn("Failed to process announcement which was part of a reconcile", nil)
					}
				}
			default:
				logger.Warn("Received unknown message type", commontypes.LogFields{"msg": v})
			}
		}
	}
}

// processAnnouncement locks lock for its whole lifetime.
func (p *discoveryProtocol) processAnnouncement(ann Announcement) error {
	p.lock.Lock()
	defer p.lock.Unlock()
	return p.lockedProcessAnnouncement(ann)
}

// lockedProcessAnnouncement requires lock to be held.
func (p *discoveryProtocol) lockedProcessAnnouncement(ann Announcement) error {
	logger := p.logger.MakeChild(commontypes.LogFields{"in": "processAnnouncement"}).MakeChild(ann.toLogFields())
	pid, err := ann.PeerID()
	if err != nil {
		return errors.Wrap(err, "failed to obtain peer id from announcement")
	}

	if p.locked.numGroupsByOracle[pid] == 0 {
		return fmt.Errorf("got announcement for an oracle we don't share a group with (%s)", pid)
	}

	err = ann.verify()
	if err != nil {
		return errors.Wrap(err, "failed to verify announcement")
	}

	if localann, exists := p.locked.bestAnnouncement[pid]; !exists || localann.Counter <= ann.Counter {
		if exists && pid != p.ownID && localann.Counter == ann.Counter {
			return nil
		}
		p.locked.bestAnnouncement[pid] = ann
		if pid == p.ownID {
			bumpedann, better, err := p.lockedBumpOwnAnnouncement()
			if err != nil {
				return errors.Wrap(err, "failed to bump own announcement")
			}

			if !better {
				return nil
			}

			logger.Info("Received better announcement for us - bumped", nil)
			select {
			case p.chInternalBump <- *bumpedann:
			case <-p.ctx.Done():
				return nil
			}
		} else {
			logger.Info("Received better announcement for peer", nil)
			select {
			case p.chConnectivity <- connectivityMsg{connectivityAdd, pid}:
			case <-p.ctx.Done():
				return nil
			}
		}
	}

	return nil
}

func (p *discoveryProtocol) sendToAllowedPeers(ann Announcement) {
	p.lock.RLock()
	allowedPeers := p.lockedAllowedPeers(ann)
	p.lock.RUnlock()
	for _, pid := range allowedPeers {
		select {
		case p.chOutgoingMessages <- outgoingMessage{ann, pid}:
		case <-p.ctx.Done():
			return
		}
	}
}

func (p *discoveryProtocol) sendLoop() {
	logger := p.logger.MakeChild(commontypes.LogFields{"in": "sendLoop"})
	logger.Debug("Entering", nil)
	defer logger.Debug("Exiting", nil)
	tick := time.After(0)
	for {
		select {
		case <-p.ctx.Done():
			return
		case ourann := <-p.chInternalBump:
			logger.Info("Our announcement was bumped - broadcasting", ourann.toLogFields())
			p.sendToAllowedPeers(ourann)
		case <-tick:
			logger.Debug("Starting reconciliation", nil)
			reconcileByPeer := make(map[ragetypes.PeerID]*reconcile)
			func() {
				p.lock.RLock()
				defer p.lock.RUnlock()
				for _, ann := range p.locked.bestAnnouncement {
					for _, pid := range p.lockedAllowedPeers(ann) {
						if _, exists := reconcileByPeer[pid]; !exists {
							reconcileByPeer[pid] = &reconcile{Anns: []Announcement{}}
						}
						r := reconcileByPeer[pid]
						r.Anns = append(r.Anns, ann)
					}
				}
			}()

			for pid, rec := range reconcileByPeer {
				select {
				case p.chOutgoingMessages <- outgoingMessage{rec, pid}:
					logger.Trace("Sending reconcile", commontypes.LogFields{"remotePeerID": pid, "reconcile": rec.toLogFields()})
				case <-p.ctx.Done():
					return
				}
			}
			tick = time.After(p.deltaReconcile)
		}
	}
}

// lockedBumpOwnAnnouncement requires lock to be held by the caller.
func (p *discoveryProtocol) lockedBumpOwnAnnouncement() (*Announcement, bool, error) {
	logger := p.logger.MakeChild(commontypes.LogFields{"in": "lockedBumpOwnAnnouncement"})
	oldann, exists := p.locked.bestAnnouncement[p.ownID]
	newctr := uint64(0)

	if exists {
		if equalAddrs(oldann.Addrs, p.ownAddrs) {
			return nil, false, nil
		}
		// Counter is uint64, and it only changes when a peer's
		// addresses change. We assume a peer will not change addresses
		// more than 2**64 times.
		newctr = oldann.Counter + 1
	}
	if newctr > announcementVersionWarnThreshold {
		logger.Warn("New announcement version too big!", commontypes.LogFields{"version": newctr})
	}
	newann := unsignedAnnouncement{Addrs: p.ownAddrs, Counter: newctr}
	sann, err := newann.sign(p.privKey)
	if err != nil {
		return nil, false, errors.Wrap(err, "failed to sign own announcement")
	}
	logger.Info("Replacing our own announcement", sann.toLogFields())
	p.locked.bestAnnouncement[p.ownID] = sann
	return &sann, true, nil
}

func (p *discoveryProtocol) Close() error {
	logger := p.logger.MakeChild(commontypes.LogFields{"in": "Close"})
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if p.state != discoveryProtocolStarted {
		return fmt.Errorf("cannot close discoveryProtocol that is not started, state was: %v", p.state)
	}
	p.state = discoveryProtocolClosed

	logger.Debug("Exiting", nil)
	defer logger.Debug("Exited", nil)
	p.ctxCancel()
	p.processes.Wait()
	return nil
}
