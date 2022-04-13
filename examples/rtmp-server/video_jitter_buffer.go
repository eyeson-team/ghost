package main

import (
	"log"
	"time"

	"github.com/pion/rtp"
)

type WebrtcSender interface {
	WriteRTP(p *rtp.Packet) error
}

type VideoJitterBuffer struct {
	packetsCh chan *rtp.Packet
}

func NewVideoJitterBuffer(sender WebrtcSender, queueLen time.Duration) (*VideoJitterBuffer, error) {
	packetsCh := make(chan *rtp.Packet, 1)
	// start jitter-loop
	go jitterLoop(packetsCh, sender, queueLen)

	return &VideoJitterBuffer{
		packetsCh: packetsCh,
	}, nil
}

func (vjb *VideoJitterBuffer) Close() {
	close(vjb.packetsCh)
}

func (vjb *VideoJitterBuffer) WriteRTP(p *rtp.Packet) error {
	vjb.packetsCh <- p
	return nil
}

func jitterLoop(packetsCh <-chan *rtp.Packet, sender WebrtcSender,
	queueLen time.Duration) {
	defer log.Println("jitterLoop done")
	worker := time.NewTicker(10 * time.Millisecond)
	defer worker.Stop()

	queue := []*rtp.Packet{}

	for {
		select {
		case p, ok := <-packetsCh:
			if !ok {
				return
			}
			queue = append(queue, p)
		case <-worker.C:
			for {
				if len(queue) < 2 {
					break
				}

				front := queue[0]
				back := queue[len(queue)-1]
				var tsDiff int64 = int64(back.Timestamp - front.Timestamp)
				if tsDiff < 0 {
					log.Println("Timestamp wrap -> flush queue")
					queue = []*rtp.Packet{}
					break
				}
				tsDiffInMs := tsDiff / 90
				//fmt.Printf("qlen: %d,  diff-in-ms: %d\n", len(queue), tsDiffInMs)

				if time.Duration(tsDiffInMs)*time.Millisecond < queueLen {
					break
				}

				err := sender.WriteRTP(front)
				if err != nil {
					log.Println("Failed to send rtp-packte:", err)
					return
				}
				queue = queue[1:]
			}
		}
	}
}
