module github.com/Syleron/PulseHA

go 1.12

replace (
	github.com/Sirupsen/logrus v1.0.5 => github.com/sirupsen/logrus v1.0.5
	github.com/Sirupsen/logrus v1.3.0 => github.com/Sirupsen/logrus v1.0.6
	github.com/Sirupsen/logrus v1.4.0 => github.com/sirupsen/logrus v1.0.6
)

require (
	github.com/Sirupsen/logrus v1.4.0
	github.com/golang/protobuf v1.3.2
	github.com/mattn/go-runewidth v0.0.4 // indirect
	github.com/mitchellh/cli v1.0.0
	github.com/olekukonko/tablewriter v0.0.1
	github.com/onsi/ginkgo v1.10.1 // indirect
	github.com/onsi/gomega v1.7.0 // indirect
	github.com/sirupsen/logrus v1.4.2 // indirect
	github.com/stretchr/testify v1.4.0 // indirect
	google.golang.org/grpc v1.23.0
	gopkg.in/airbrake/gobrake.v2 v2.0.9 // indirect
	gopkg.in/gemnasium/logrus-airbrake-hook.v2 v2.1.2 // indirect
)
