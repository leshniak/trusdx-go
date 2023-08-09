package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tarm/serial"
)

type SerialStream struct {
	AudioOutBuf     chan []byte
	AudioInBuf      chan []byte
	RepliesBuf      chan []byte
	CmdsBuf         chan []byte
	port            *serial.Port
	serialConfig    *serial.Config
	isStreamingMode bool
	isTransmitting  bool
	chunkLength     int
	isRunning       bool
}

func NewSerialStream(name string) *SerialStream {
	ss := new(SerialStream)
	ss.isStreamingMode = false
	ss.isTransmitting = false
	ss.chunkLength = 48
	ss.AudioOutBuf = make(chan []byte, 128)
	ss.AudioInBuf = make(chan []byte, 128)
	ss.RepliesBuf = make(chan []byte, 32)
	ss.CmdsBuf = make(chan []byte, 32)
	ss.serialConfig = &serial.Config{Name: name, Baud: 115200}
	port, err := serial.OpenPort(ss.serialConfig)
	if err != nil {
		log.Fatalln(err)
	}
	ss.port = port

	return ss
}

func (ss *SerialStream) Start() {
	ss.isRunning = true
	go ss.receiveDataStream()
	go ss.sendDataStream()
}

func (ss *SerialStream) handleDataChunk(buffer *bytes.Buffer) {
	if buffer.Len() == 0 {
		return
	}

	data, err := buffer.ReadBytes(';')
	if errors.Is(err, io.EOF) && len(data) < ss.chunkLength && !ss.isStreamingMode {
		buffer.Write(data)
		return
	}

	if ss.isStreamingMode {
		dataNoDelim, hasDelim := bytes.CutSuffix(data, []byte(";"))
		ss.AudioOutBuf <- dataNoDelim
		ss.isStreamingMode = !hasDelim
		return
	}

	ss.isStreamingMode = bytes.HasPrefix(data, []byte("US"))
	if ss.isStreamingMode {
		dataNoDelim, _ := bytes.CutSuffix(data[2:], []byte(";"))
		ss.AudioOutBuf <- dataNoDelim
		return
	}

	ss.RepliesBuf <- data
}

func (ss *SerialStream) receiveDataStream() {
	buffer := bytes.NewBuffer(make([]byte, ss.chunkLength))
	buffer.Reset()

	for ss.isRunning {
		chunk := make([]byte, ss.chunkLength)
		readCount, err := ss.port.Read(chunk)
		if err != nil {
			log.Fatalln(err)
		}
		buffer.Write(chunk[:readCount])
		ss.handleDataChunk(buffer)
	}
}

func (ss *SerialStream) sendDataStream() {
	for ss.isRunning {
		select {
		case cmd := <-ss.CmdsBuf:
			if ss.isTransmitting {
				time.Sleep(10 * time.Millisecond)
				ss.port.Write([]byte(";"))
				// fmt.Print(";")
				ss.port.Flush()
			}

			if bytes.HasPrefix(cmd, []byte("RX")) {
				ss.isTransmitting = false
				log.Debugf("[RX Mode]")
			}

			cmd = append(cmd, ';')
			ss.port.Write(cmd)
			// fmt.Printf("%s", cmd)
			ss.port.Flush()

			if bytes.HasPrefix(cmd, []byte("TX")) {
				ss.isTransmitting = true
				time.Sleep(10 * time.Millisecond)
				log.Debugf("[TX Mode]")
			}
		case samples := <-ss.AudioInBuf:
			if ss.isTransmitting {
				samples = bytes.ReplaceAll(samples, []byte{0x3b}, []byte{0x3a})
				ss.port.Write([]byte(samples))
				// fmt.Printf("%s", []byte(samples))
				ss.port.Flush()
			}
		}
	}
}

func (ss *SerialStream) PushCommand(cmdString string) {
	cmds := strings.Split(cmdString, ";")
	for i, cmd := range cmds {
		if cmd != "" || i == 0 {
			if strings.HasPrefix(cmd, "ID") {
				// send a reply without bothering a rig, the reply is constant anyway
				// this is a workaround for unrealistic fast RTT expectations in hamlib for sequence RX;ID;
				ss.RepliesBuf <- []byte("ID020;")
			} else {
				ss.CmdsBuf <- []byte(cmd)
			}
		}
	}
}

func (ss *SerialStream) Close() {
	time.Sleep(50 * time.Millisecond)
	ss.isRunning = false
	time.Sleep(50 * time.Millisecond)
	ss.port.Flush()
	ss.port.Close()
}
