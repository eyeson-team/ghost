module rtmp-server

go 1.16

require (
	github.com/aler9/gortsplib v0.0.0-20220320114528-0e8c93c5c2ea
	github.com/eyeson-team/eyeson-go v1.1.2-0.20220308152915-af4b016488d5
	github.com/eyeson-team/ghost/v2 v2.0.1
	github.com/icza/bitio v1.1.0 // indirect
	github.com/pion/rtp v1.7.4
	github.com/pion/rtp/v2 v2.0.0-20220302185659-b3d10fc096b0
	github.com/spf13/cobra v1.3.0
	golang.org/x/net v0.0.0-20220225172249-27dd8689420f // indirect
	golang.org/x/sys v0.0.0-20220319134239-a9b59b0215f8 // indirect
)

// use our forked version
//replace github.com/aler9/gortsplib => github.com/eyeson-team/gortsplib v0.0.0-20220309162612-01a71277176d
