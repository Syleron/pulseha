package hcSerial

import (
	"encoding/hex"
	"fmt"
	"github.com/syleron/pulseha/plugins/hcSerial/packages/serial"
	"io"
)

type HcSerial struct {
	rwc io.ReadWriteCloser
}

// Open setups up serial object and connection
func Open() (*HcSerial, error) {
	options := serial.ConnectionOptions{
		PortName:                "",
		BaudRate:                9600,
		DataBits:                0,
		StopBits:                0,
		ParityMode:              0,
		RTSCTSFlowControl:       false,
		InterCharacterTimeout:   0,
		MinimumReadSize:         0,
		Rs485Enable:             false,
		Rs485RtsHighDuringSend:  false,
		Rs485RtsHighAfterSend:   false,
		Rs485RxDuringTx:         false,
		Rs485DelayRtsBeforeSend: 0,
		Rs485DelayRtsAfterSend:  0,
	}
	s, err := serial.Open(options)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	hcs := &HcSerial{
		rwc: s,
	}
	hcs.setupListener()
	return hcs, nil
}

// Write writes to our connection.
func (hs *HcSerial) Write(b []byte) (int, error) {
	// bCount is the number of bytes written
	bCount, err := hs.rwc.Write(b)
	return bCount, err
}

// setupListener listens for incoming messages.
func (hs *HcSerial) setupListener() {
	buf := make([]byte, 32)
	n, err := hs.rwc.Read(buf)
	if err != nil {
		if err != io.EOF {
			fmt.Println("Error reading from serial port: ", err)
		}
	} else {
		buf = buf[:n]
		fmt.Println("Rx: ", hex.EncodeToString(buf))
	}
}
