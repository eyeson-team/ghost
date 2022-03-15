module rtmp-server

go 1.16

require (
	github.com/aler9/gortsplib v0.0.0-20220308161130-58557ecd5e2b
	github.com/eyeson-team/eyeson-go v1.1.2-0.20220308152915-af4b016488d5
	github.com/eyeson-team/ghost/v2 v2.0.1
	github.com/notedit/rtmp v0.0.2
	github.com/spf13/cobra v1.3.0
)

// use our forked version
replace github.com/aler9/gortsplib => github.com/eyeson-team/gortsplib v0.0.0-20220309162612-01a71277176d
