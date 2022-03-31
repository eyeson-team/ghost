package main

import (
	"os"
)

type DotH264 struct {
	file *os.File
}

func NewDotH264(filename string) (*DotH264, error) {
	file, err := os.Create(filename)
	if err != nil {
		return nil, err
	}
	return &DotH264{file: file}, nil
}

func (doth *DotH264) WriteNalus(nalus [][]byte) error {

	beacon := make([]byte, 4)
	beacon[3] = 0x01

	for _, n := range nalus {
		_, err := doth.file.Write(beacon)
		if err != nil {
			return err
		}
		_, err = doth.file.Write(n)
		if err != nil {
			return err
		}
		// Writing beacon twice. Without some PPS seam to
		// be not correctly read.
		_, err = doth.file.Write(beacon)
		if err != nil {
			return err
		}

	}
	return nil
}
