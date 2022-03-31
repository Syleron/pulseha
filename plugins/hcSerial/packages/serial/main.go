package serial

import "io"

// ConnectionOptions Defines our structure options for a connection
type ConnectionOptions struct {
	// The name of our port e.g. tty01
	PortName string
	// The baud rate for the port.
	BaudRate uint
	// The number of bits per frame.
	// Legal values: 5, 6, 7, 8
	DataBits uint
	// The number of stop bits per frame.
	// Legal values: 1, 2
	StopBits uint
	// Parity bits type for the connection.
	ParityMode ParityMode
	// Enable hardware flow control.
	RTSCTSFlowControl     bool
	InterCharacterTimeout uint
	MinimumReadSize       uint

	// Enable RS485 mode. TODO: Might need this later...
	Rs485Enable             bool
	Rs485RtsHighDuringSend  bool
	Rs485RtsHighAfterSend   bool
	Rs485RxDuringTx         bool
	Rs485DelayRtsBeforeSend int
	Rs485DelayRtsAfterSend  int
}

// Open internal connection using connection options object.
func Open(co ConnectionOptions) (io.ReadWriteCloser, error) {
	return setup(co)
}
