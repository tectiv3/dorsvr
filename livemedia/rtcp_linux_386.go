package livemedia

import (
	sys "syscall"
	"time"

	gs "github.com/tectiv3/dorsvr/groupsock"
	"github.com/tectiv3/gitea/log"
)

const (
	// RTCP packet types:
	RTCP_PT_SR   = 200
	RTCP_PT_RR   = 201
	RTCP_PT_SDES = 202
	RTCP_PT_BYE  = 203
	RTCP_PT_APP  = 204

	// SDES tags:
	RTCP_SDES_END   = 0
	RTCP_SDES_CNAME = 1
	RTCP_SDES_NAME  = 2
	RTCP_SDES_EMAIL = 3
	RTCP_SDES_PHONE = 4
	RTCP_SDES_LOC   = 5
	RTCP_SDES_TOOL  = 6
	RTCP_SDES_NOTE  = 7
	RTCP_SDES_PRIV  = 8

	// overhead (bytes) of IP and UDP hdrs
	IP_UDP_HDR_SIZE = 28

	PACKET_UNKNOWN_TYPE = 0
	PAKCET_RTP          = 1
	PACKET_RTCP_REPORT  = 2
	PACKET_BYE          = 3
)

type SDESItem struct {
	data []byte
}

// bytes, (1500, minus some allowance for IP, UDP, UMTP headers)
const (
	maxRTCPPacketSize   uint = 1450
	preferredPacketSize uint = 1000 // bytes
)

type RTCPInstance struct {
	typeOfEvent          uint
	lastSentSize         uint
	totSessionBW         uint
	numMembers           uint
	lastPacketSentSize   uint
	avgRTCPSize          float64
	haveJustSentPacket   bool
	prevReportTime       int32
	nextReportTime       int32
	inBuf                []byte
	CNAME                *SDESItem
	Sink                 IMediaSink
	Source               *RTPSource
	outBuf               *OutPacketBuffer
	netInterface         *RTPInterface
	byeHandlerTask       interface{}
	SRHandlerTask        interface{}
	RRHandlerTask        interface{}
	byeHandlerClientData interface{}
}

func newSDESItem(tag int, value string) *SDESItem {
	length := len(value)
	if length > 0xFF {
		length = 0xFF // maximum data length for a SDES item
	}

	return &SDESItem{
		data: []byte{byte(tag), byte(length)},
	}
}

func dTimeNow() int32 {
	var timeNow sys.Timeval
	sys.Gettimeofday(&timeNow)
	return timeNow.Sec + timeNow.Usec/1000000.0
}

func (s *SDESItem) totalSize() uint {
	return 2 + uint(s.data[1])
}

func newRTCPInstance(rtcpGS *gs.GroupSock, totSessionBW uint, cname string,
	sink IMediaSink, source *RTPSource) *RTCPInstance {
	// saved OutPacketBuffer's max size temporarily
	savedMaxSize := OutPacketBufferMaxSize
	OutPacketBufferMaxSize = maxRTCPPacketSize

	reportTime := dTimeNow()
	rtcp := &RTCPInstance{
		typeOfEvent:    eventReport,
		totSessionBW:   totSessionBW,
		prevReportTime: reportTime,
		nextReportTime: reportTime,
		CNAME:          newSDESItem(RTCP_SDES_CNAME, cname),
		outBuf:         newOutPacketBuffer(preferredPacketSize, maxRTCPPacketSize),
		inBuf:          make([]byte, maxRTCPPacketSize),
		Sink:           sink,
		Source:         source,
	}
	// resume common OutPacketBuffer's max size
	OutPacketBufferMaxSize = savedMaxSize

	if rtcp.totSessionBW == 0 {
		log.Warn("[newRTCPInstance] totSessionBW can't be zero!")
		rtcp.totSessionBW = 1
	}

	rtcp.netInterface = newRTPInterface(rtcp, rtcpGS)
	rtcp.netInterface.startNetworkReading(rtcp.incomingReportHandler)

	rtcp.onExpire()
	return rtcp
}

func (r *RTCPInstance) NumMembers() uint {
	return r.numMembers
}

func (r *RTCPInstance) sentPacketSize() uint {
	return r.lastSentSize
}

func (r *RTCPInstance) setSpecificRRHandler(handlerTask interface{}) {
}

func (r *RTCPInstance) SetByeHandler(handlerTask interface{}, clientData interface{}) {
	r.byeHandlerTask = handlerTask
	r.byeHandlerClientData = clientData
}

func (r *RTCPInstance) setSRHandler(handlerTask interface{}, clientData interface{}) {
	r.SRHandlerTask = handlerTask
}

func (r *RTCPInstance) setRRHandler(handlerTask interface{}, clientData interface{}) {
	r.RRHandlerTask = handlerTask
}

func (r *RTCPInstance) incomingReportHandler() {
	for {
		readBytes, err := r.netInterface.handleRead(r.inBuf)
		if err != nil {
			log.Error(4, "failed to read.%v", err)
			break
		}

		packetSize := uint(readBytes)

		r.processIncomingReport(packetSize)
	}
	log.Info("incomingReportHandler ending.")
}

func (r *RTCPInstance) processIncomingReport(packetSize uint) {
	var fromAddress string
	var callByeHandler bool

	packet := r.inBuf[:packetSize]

	totPacketSize := IP_UDP_HDR_SIZE + packetSize

	if packetSize < 4 {
		log.Warn("RTCP Interface packet Size less than 4.")
		return
	}

	var rtcpHdr uint32
	rtcpHdr, _ = gs.Ntohl(packet)

	if (rtcpHdr & 0xE0FE0000) != (0x80000000 | (RTCP_PT_SR << 16)) {
		log.Warn("rejected bad RTCP packet: header 0x%08x", rtcpHdr)
		return
	}

	typeOfPacket := PACKET_UNKNOWN_TYPE
	var packetOk bool
	var reportSenderSSRC uint32

	for {
		rc := (rtcpHdr >> 24) & 0x1F
		pt := (rtcpHdr >> 16) & 0xFF
		log.Warn("pt: %v", pt)
		// doesn't count hdr
		length := uint(4 * (rtcpHdr & 0xFFFF))
		// skip over the header
		packet, packetSize = ADVANCE(packet, packetSize, 4)
		if length > packetSize {
			break
		}

		// Assume that each RTCP subpacket begins with a 4-byte SSRC:
		if length < 4 {
			break
		}
		length -= 4

		reportSenderSSRC, _ = gs.Ntohl(packet)

		packet, packetSize = ADVANCE(packet, packetSize, 4)

		var subPacketOk bool
		switch pt {
		case RTCP_PT_SR:
			if length >= 20 {
				length -= 20

				// Extract the NTP timestamp, and note this:
				ntpMsw, _ := gs.Ntohl(packet)
				packet, packetSize = ADVANCE(packet, packetSize, 4)

				ntpLsm, _ := gs.Ntohl(packet)
				packet, packetSize = ADVANCE(packet, packetSize, 4)

				rtpTimestamp, _ := gs.Ntohl(packet)
				packet, packetSize = ADVANCE(packet, packetSize, 4)

				if r.Source != nil {
					r.Source.receptionStatsDB.noteIncomingSR(reportSenderSSRC, ntpMsw, ntpLsm, rtpTimestamp)
				}

				packet, packetSize = ADVANCE(packet, packetSize, 8)

				// If a 'SR handler' was set, call it now:
				if r.SRHandlerTask != nil {
					r.SRHandlerTask.(func())()
				}
			}

			fallthrough
		case RTCP_PT_RR:
			reportBlocksSize := uint(rc * (6 * 4))
			if length >= reportBlocksSize {
				length -= reportBlocksSize

				if r.Sink != nil {
					// Use this information to update stats about our transmissions:
					transmissionStats := r.Sink.transmissionStatsDB()
					var i, jitter, lossStats, timeLastSR, timeSinceLastSR, highestReceived uint32
					for i = 0; i < rc; i++ {
						senderSSRC, _ := gs.Ntohl(packet)
						packet, packetSize = ADVANCE(packet, packetSize, 4)
						// We care only about reports about our own transmission, not others'
						if senderSSRC == r.Sink.ssrc() {
							lossStats, _ = gs.Ntohl(packet)
							packet, packetSize = ADVANCE(packet, packetSize, 4)
							highestReceived, _ = gs.Ntohl(packet)
							packet, packetSize = ADVANCE(packet, packetSize, 4)
							jitter, _ = gs.Ntohl(packet)
							packet, packetSize = ADVANCE(packet, packetSize, 4)
							timeLastSR, _ = gs.Ntohl(packet)
							packet, packetSize = ADVANCE(packet, packetSize, 4)
							timeSinceLastSR, _ = gs.Ntohl(packet)
							packet, packetSize = ADVANCE(packet, packetSize, 4)
							transmissionStats.noteIncomingRR(fromAddress, reportSenderSSRC,
								lossStats, highestReceived, jitter, timeLastSR, timeSinceLastSR)
						} else {
							packet, packetSize = ADVANCE(packet, packetSize, 4*5)
						}
					}
				} else {
					packet, packetSize = ADVANCE(packet, packetSize, reportBlocksSize)
				}

				if pt == RTCP_PT_RR {
					log.Info("RTCP_PT_RR")
					if r.RRHandlerTask != nil {
						r.RRHandlerTask.(func())()
					}
				}

				subPacketOk = true
				typeOfPacket = PACKET_RTCP_REPORT
			}

		case RTCP_PT_BYE:
			log.Info("RTCP_PT_BYE")
			callByeHandler = true

			subPacketOk = true
			typeOfPacket = PACKET_BYE
		default:
			subPacketOk = true
		}

		if !subPacketOk {
			break
		}

		packet, packetSize = ADVANCE(packet, packetSize, length)

		if packetSize == 0 {
			packetOk = true
			break
		} else if packetSize < 4 {
			log.Error(4, "extraneous %d bytes at end of RTCP packet!", packetSize)
			break
		}

		rtcpHdr, _ = gs.Ntohl(packet)

		if (rtcpHdr & 0xC0000000) != 0x80000000 {
			log.Error(4, "bad RTCP subpacket: header 0x%08x", rtcpHdr)
			break
		}
	}

	if !packetOk {
		log.Warn("rejected bad RTCP subpacket: header 0x%08x", rtcpHdr)
		return
	} else {
		log.Info("validated entire RTCP packet")
	}

	r.onReceive(typeOfPacket, totPacketSize, uint(reportSenderSSRC))

	if callByeHandler && r.byeHandlerTask != nil {
		r.byeHandlerTask.(func(subsession *MediaSubsession))(r.byeHandlerClientData.(*MediaSubsession))
	}
}

func (r *RTCPInstance) onReceive(typeOfPacket int, totPacketSize, ssrc uint) {
	OnReceive()
}

func (r *RTCPInstance) sendReport() {
	// Begin by including a SR and/or RR report:
	r.addReport()

	// Then, include a SDES:
	r.addSDES()

	// Send the report:
	r.sendBuiltPacket()
}

func (r *RTCPInstance) sendBye() {
	r.addReport()

	r.addBYE()
	r.sendBuiltPacket()
}

func (r *RTCPInstance) sendBuiltPacket() {
	reportSize := r.outBuf.curPacketSize()
	r.netInterface.sendPacket(r.outBuf.packet(), reportSize)
	r.outBuf.resetOffset()

	r.lastSentSize = uint(IP_UDP_HDR_SIZE) + reportSize
	r.haveJustSentPacket = true
	r.lastPacketSentSize = reportSize
}

func (r *RTCPInstance) addReport() {
	if r.Sink != nil {
		if r.Sink.enableRTCPReports() {
			return
		}

		if r.Sink.nextTimestampHasBeenPreset() {
			return
		}

		r.addSR()
	} else if r.Source != nil {
		r.addRR()
	}
}

func (r *RTCPInstance) addSDES() {
	var numBytes uint = 4
	numBytes += r.CNAME.totalSize()
	numBytes += 1

	num4ByteWords := (numBytes + 3) / 4

	var rtcpHdr uint32 = 0x81000000 // version 2, no padding, 1 SSRC chunk
	rtcpHdr |= (RTCP_PT_SDES << 16)
	rtcpHdr |= uint32(num4ByteWords)
	r.outBuf.enqueueWord(rtcpHdr)
}

func (r *RTCPInstance) addBYE() {
	var rtcpHdr uint32 = 0x81000000
	rtcpHdr |= RTCP_PT_BYE << 16
	rtcpHdr |= 1
	r.outBuf.enqueueWord(rtcpHdr)

	if r.Source != nil {
		r.outBuf.enqueueWord(uint32(r.Source.ssrc))
	} else if r.Sink != nil {
		r.outBuf.enqueueWord(uint32(r.Sink.ssrc()))
	}
}

func (r *RTCPInstance) addSR() {
	r.enqueueCommonReportPrefix(RTCP_PT_SR, r.Source.ssrc, 0)
	r.enqueueCommonReportSuffix()
}

func (r *RTCPInstance) addRR() {
	r.enqueueCommonReportPrefix(RTCP_PT_RR, r.Source.ssrc, 0)
	r.enqueueCommonReportSuffix()
}

func (r *RTCPInstance) schedule(nextTime int64) {
	r.nextReportTime = int32(nextTime)
	secondsToDelay := int32(nextTime) - dTimeNow()
	if secondsToDelay < 0 {
		secondsToDelay = 0
	}
	usToGo := secondsToDelay * 1000000000
	time.Sleep(time.Duration(usToGo) * time.Microsecond)
	go r.onExpire()
}

func (r *RTCPInstance) onExpire() {
	// Note: totsessionbw is kbits per second
	var rtcpBW float32 = 0.05 * float32(r.totSessionBW) * 1024 / 8

	var senders float64
	if r.Sink != nil {
		senders = 1
	}

	OnExpire(r, float64(r.NumMembers()), senders, senders, float64(rtcpBW), r.avgRTCPSize, float64(dTimeNow()), float64(r.prevReportTime))
}

func (r *RTCPInstance) unsetSpecificRRHandler() {
}

func (r *RTCPInstance) enqueueCommonReportPrefix(packetType, ssrc, numExtraWords uint32) {
}

func (r *RTCPInstance) enqueueCommonReportSuffix() {
}

func (r *RTCPInstance) destroy() {
	r.netInterface.stopNetworkReading()

	r.sendBye()
}
