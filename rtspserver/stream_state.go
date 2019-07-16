package rtspserver

import "github.com/tectiv3/dorsvr/livemedia"

type StreamServerState struct {
	subsession  livemedia.IServerMediaSubsession
	streamToken *livemedia.StreamState
}
