package main

import (
	"bytes"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gordonklaus/portaudio"
	"github.com/pkg/term/termios"
	log "github.com/sirupsen/logrus"
	"github.com/tarm/serial"
	"golang.org/x/sys/unix"
)

const dataChunkLength = 48

var isRunning = true

func getAudioFromRig(stream *portaudio.Stream, rcvdAudio chan []byte, streamBuf *[]uint8) {
	silenceSamples := make([]uint8, len(*streamBuf))

	for i := 0; i < len(silenceSamples); i++ {
		silenceSamples[i] = 128
	}

	for isRunning {
		select {
		case samples := <-rcvdAudio:
			copy(*streamBuf, samples)
		default:
			copy(*streamBuf, silenceSamples)
		}

		err := stream.Write()
		if errors.Is(err, portaudio.StreamIsStopped) {
			continue
		} else if err != nil {
			panic(err)
		}
	}
}

func pushAudioToRig(s *portaudio.Stream, sndAudio chan []byte, streamBuf *[]uint8) {
	for isRunning {
		toRead, err := s.AvailableToRead()
		if toRead <= 0 || err != nil {
			continue
		}
		err = s.Read()
		if errors.Is(err, portaudio.StreamIsStopped) {
			continue
		} else if err != nil {
			panic(err)
		}
		samples := make([]byte, len(*streamBuf))
		copy(samples, *streamBuf)
		sndAudio <- samples
	}
}

func tty2tty(src *os.File, dst *os.File) {
	for isRunning {
		buffer := make([]byte, 64)
		readCount, _ := src.Read(buffer)
		dst.Write(buffer[:readCount])
	}
}

func sendCatToPort(port *serial.Port, ss *SerialStream) {
	for isRunning {
		cmd := <-ss.RepliesBuf
		log.Debugf("[CAT <- Rig]: %s\n", cmd)
		port.Write([]byte(cmd))
	}
}

func getCatFromPort(port *serial.Port, ss *SerialStream) {
	const bufferSize = 64

	for isRunning {
		buffer := make([]byte, bufferSize)
		readCount, _ := port.Read(buffer)
		if readCount > 0 {
			cmdString := bytes.NewBuffer(buffer[:readCount]).String()
			log.Debugf("[CAT -> Rig]: %s\n", cmdString)
			ss.PushCommand(cmdString)
		}
	}
}

func configurePort(port *os.File) {
	attrs := unix.Termios{}
	termios.Tcgetattr(port.Fd(), &attrs)
	attrs.Lflag &^= unix.ECHO | unix.ECHOE | unix.ECHOKE | unix.ECHOCTL | unix.HUPCL
	attrs.Ispeed = unix.B115200
	attrs.Ospeed = unix.B115200
	termios.Tcsetattr(port.Fd(), termios.TCSANOW, &attrs)
}

func setLogLevel() {
	levelText, ok := os.LookupEnv("LOG_LEVEL")

	if !ok {
		levelText = "info"
	}

	logLevel, err := log.ParseLevel(levelText)
	if err != nil {
		logLevel = log.InfoLevel
	}

	log.SetLevel(logLevel)
}

func main() {
	sig := make(chan os.Signal, 1)
	done := make(chan bool, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	setLogLevel()

	devicePort := "/dev/tty.wchusbserial110"

	devicePortFile, err := os.OpenFile(devicePort, os.O_RDWR|syscall.O_NONBLOCK, os.ModeDevice)
	if err != nil {
		log.Fatalln(err)
	}
	configurePort(devicePortFile)
	devicePortFile.Close()

	ss := NewSerialStream(devicePort)
	log.Println("Warming up, please wait...")
	time.Sleep(3 * time.Second)
	ss.Start()
	log.Println("Driver ready! Press Ctrl-C to stop.")

	ptmCat, ptsCat, _ := termios.Pty()
	ptmLoop, ptsLoop, _ := termios.Pty()
	log.Printf("CAT serial port: %s\n", ptsCat.Name())
	serialConfig := &serial.Config{Name: ptsLoop.Name(), Baud: 115200}
	port, err := serial.OpenPort(serialConfig)
	if err != nil {
		log.Fatalln(err)
	}
	configurePort(ptsCat)
	configurePort(ptsLoop)
	go tty2tty(ptmCat, ptmLoop)
	go tty2tty(ptmLoop, ptmCat)
	go getCatFromPort(port, ss)
	go sendCatToPort(port, ss)

	portaudio.Initialize()
	paHost, err := portaudio.DefaultHostApi()
	if err != nil {
		log.Fatalln(err)
	}
	defer portaudio.Terminate()

	outStreamParams := portaudio.LowLatencyParameters(nil, paHost.Devices[1])
	outStreamParams.Output.Channels = 1
	outStreamParams.SampleRate = 7820
	outStreamParams.FramesPerBuffer = dataChunkLength
	outStreamBuf := make([]uint8, dataChunkLength)
	outStream, err := portaudio.OpenStream(outStreamParams, &outStreamBuf)
	if err != nil {
		log.Fatalln(err)
	}

	inStreamParams := portaudio.LowLatencyParameters(paHost.Devices[1], nil)
	inStreamParams.Output.Channels = 1
	inStreamParams.SampleRate = 11520
	inStreamParams.FramesPerBuffer = dataChunkLength
	inStreamBuf := make([]uint8, dataChunkLength)
	inStream, err := portaudio.OpenStream(outStreamParams, &inStreamBuf)
	if err != nil {
		log.Fatalln(err)
	}

	go getAudioFromRig(outStream, ss.AudioOutBuf, &outStreamBuf)
	go pushAudioToRig(inStream, ss.AudioInBuf, &inStreamBuf)
	outStream.Start()
	inStream.Start()

	ss.PushCommand(";MD2;UA2;RX;")

	go func() {
		<-sig
		isRunning = false
		ss.PushCommand(";UA0;")
		ss.Close()
		port.Close()
		outStream.Close()
		inStream.Close()
		log.Println("Bye-bye!")
		done <- true
	}()

	<-done
}
