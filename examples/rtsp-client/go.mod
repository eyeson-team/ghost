module rtsp-client

go 1.16

require (
	github.com/aler9/gortsplib v0.0.0-20220308161130-58557ecd5e2b
	//github.com/aler9/gortsplib v0.0.0-20220324142719-7d9c882cc95b
	github.com/eyeson-team/eyeson-go v1.1.2-0.20220308152915-af4b016488d5
	github.com/eyeson-team/ghost/v2 v2.0.4-0.20220322104449-5b6136418403
	github.com/gorilla/websocket v1.5.0 // indirect
	github.com/icza/bitio v1.1.0 // indirect
	github.com/pion/rtp v1.7.9
	github.com/pion/rtp/v2 v2.0.0-20220302185659-b3d10fc096b0 // indirect
	github.com/pion/webrtc/v3 v3.1.26 // indirect
	github.com/spf13/cobra v1.3.0
	golang.org/x/crypto v0.0.0-20220321153916-2c7772ba3064 // indirect
	golang.org/x/sys v0.0.0-20220319134239-a9b59b0215f8 // indirect
)

// use our forked version
replace github.com/aler9/gortsplib => github.com/eyeson-team/gortsplib v0.0.0-20220309162612-01a71277176d
