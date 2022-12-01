/*
	Copyright NetFoundry Inc.

	Licensed under the Apache License, Version 2.0 (the "License");
	you may not use this file except in compliance with the License.
	You may obtain a copy of the License at

	https://www.apache.org/licenses/LICENSE-2.0

	Unless required by applicable law or agreed to in writing, software
	distributed under the License is distributed on an "AS IS" BASIS,
	WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
	See the License for the specific language governing permissions and
	limitations under the License.
*/

package demo

import (
	"bufio"
	"fmt"
	"github.com/michaelquigley/pfxlog"
	"github.com/openziti/agent"
	"github.com/openziti/channel/v2"
	"github.com/openziti/identity"
	"github.com/openziti/sdk-golang/ziti"
	"github.com/openziti/sdk-golang/ziti/config"
	"github.com/openziti/sdk-golang/ziti/edge"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	EchoServerAppId            = 100
	EchoServerUpdateTerminator = 1000
	EchoServerPrecedenceHeader = 1000
	EchoServerCostHeader       = 1001
)

type echoServer struct {
	port             uint16
	healthCheckAddr  string
	verbose          bool
	logFormatter     string
	configFile       string
	service          string
	bindWithIdentity bool
	zitiIdentity     identity.Identity
	zitiListener     edge.Listener
}

func newEchoServerCmd() *cobra.Command {
	server := &echoServer{}

	cmd := &cobra.Command{
		Use:   "echo-server",
		Short: "Runs a simple network echo service",
		Args:  cobra.ExactArgs(0),
		Run:   server.run,
	}

	// allow interspersing positional args and flags
	cmd.Flags().SetInterspersed(true)
	cmd.Flags().Uint16VarP(&server.port, "port", "p", 0, "Specify the port to listen on. If not specified the TCP listener won't be started")
	cmd.Flags().StringVar(&server.healthCheckAddr, "health-check-addr", "", "Specify the ip:port on which to serve the health check endpoint. If not specified, a health check endpoint will not be started")
	cmd.Flags().BoolVarP(&server.verbose, "verbose", "v", false, "Enable verbose logging")
	cmd.Flags().StringVar(&server.logFormatter, "log-formatter", "", "Specify log formatter [json|pfxlog|text]")
	cmd.Flags().StringVarP(&server.configFile, "identity", "i", "", "Specify the Ziti identity to use. If not specified the Ziti listener won't be started")
	cmd.Flags().StringVarP(&server.service, "service", "s", "echo", "Ziti service to bind. Defaults to 'echo'")
	cmd.Flags().BoolVar(&server.bindWithIdentity, "addressable", false, "Specify if this application should be addressable by the edge identity")
	cmd.Flags().SetInterspersed(true)

	return cmd
}

func (self *echoServer) initLogging() {
	logLevel := logrus.InfoLevel
	if self.verbose {
		logLevel = logrus.DebugLevel
	}

	options := pfxlog.DefaultOptions().SetTrimPrefix("github.com/openziti/").NoColor()
	pfxlog.GlobalInit(logLevel, options)

	switch self.logFormatter {
	case "pfxlog":
		pfxlog.SetFormatter(pfxlog.NewFormatter(pfxlog.DefaultOptions().SetTrimPrefix("github.com/openziti/").StartingToday()))
	case "json":
		pfxlog.SetFormatter(&logrus.JSONFormatter{TimestampFormat: "2006-01-02T15:04:05.000Z"})
	case "text":
		pfxlog.SetFormatter(&logrus.TextFormatter{})
	default:
		// let logrus do its own thing
	}
}

func (self *echoServer) run(*cobra.Command, []string) {
	self.initLogging()

	log := pfxlog.Logger()

	if self.port == 0 && self.configFile == "" {
		log.Fatalf("No port specified and no ziti identity specified. Not starting either listener, so nothing to do. exiting.")
	}

	if self.healthCheckAddr != "" {
		go func() {
			err := http.ListenAndServe(self.healthCheckAddr, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(200)
			}))
			if err != nil {
				log.WithError(err).Fatalf("unable to start health check endpoint on addr [%v]", self.healthCheckAddr)
			}
		}()
	}

	if self.port > 0 {
		addr := fmt.Sprintf("0.0.0.0:%v", self.port)
		log.Infof("tcp: starting listener on address [%v]", addr)
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			log.WithError(err).Fatalf("unable to start TCP listener on addr [%v]", addr)
		}
		go self.accept("tcp", listener)
	}

	if self.configFile != "" {
		zitiConfig, err := config.NewFromFile(self.configFile)
		if err != nil {
			log.WithError(err).Fatalf("ziti: unable to load ziti identity from [%v]", self.configFile)
		}
		self.zitiIdentity, err = identity.LoadIdentity(zitiConfig.ID)
		if err != nil {
			log.WithError(err).Fatalf("ziti: unable to create ziti identity from [%v]", self.configFile)
		}

		zitiContext := ziti.NewContextWithConfig(zitiConfig)

		zitiIdentity, err := zitiContext.GetCurrentIdentity()
		if err != nil {
			log.WithError(err).Fatal("unable to get current ziti identity from controller")
		}

		listenOptions := ziti.DefaultListenOptions()
		listenOptions.BindUsingEdgeIdentity = self.bindWithIdentity
		listenOptions.Cost = zitiIdentity.DefaultHostingCost
		listenOptions.Precedence = ziti.GetPrecedenceForLabel(zitiIdentity.DefaultHostingPrecedence)

		svc, found := zitiContext.GetService(self.service)
		if !found {
			log.WithError(err).Fatalf("ziti: unable to lookup service [%v]", self.service)
		}

		if cost, found := zitiIdentity.ServiceHostingCosts[svc.Id]; found {
			listenOptions.Cost = uint16(cost.(float64))
		}

		if precedence, found := zitiIdentity.ServiceHostingPrecedences[svc.Id]; found {
			listenOptions.Precedence = ziti.GetPrecedenceForLabel(precedence.(string))
		}

		log.Infof("ziti: hosting %v with addressable=%v, cost=%v, precedence=%v",
			self.service, self.bindWithIdentity, listenOptions.Cost, listenOptions.Precedence)
		listener, err := zitiContext.ListenWithOptions(self.service, listenOptions)
		if err != nil {
			log.WithError(err).Fatalf("ziti: failed to host service [%v]", self.service)
		}

		self.zitiListener = listener

		shutdownClean := false
		agentOptions := agent.Options{
			ShutdownCleanup: &shutdownClean,
			CustomOps: map[byte]func(conn net.Conn) error{
				agent.CustomOpAsync: self.HandleCustomAgentAsyncOp,
			},
		}

		if err = agent.Listen(agentOptions); err != nil {
			log.WithError(err).Warn("failed to start agent")
		}

		go self.accept("ziti", listener)
	}

	waitC := make(chan struct{})
	<-waitC
}

func (self *echoServer) accept(connType string, listener net.Listener) {
	log := pfxlog.Logger()

	for {
		conn, err := listener.Accept()
		if err != nil {
			panic(err)
		}
		log.Infof("%v: connection received: %v", connType, conn)
		go self.echo(connType, conn)
	}
}

func (self *echoServer) echo(connType string, conn io.ReadWriteCloser) {
	log := pfxlog.Logger()

	defer func() {
		if err := conn.Close(); err != nil {
			log.WithError(err).Errorf("%v: Error while closing connection", connType)
		}
		log.Infof("%v: connection closed", connType)
	}()

	for {
		buf := make([]byte, 1024)
		n, err := conn.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				log.Infof("%v: received eof, closing connection", connType)
			} else {
				log.WithError(err).Errorf("%v: error while reading", connType)
			}
			break
		}
		log.Infof("%v: read %v bytes", connType, n)
		writeBuf := buf[:n]
		if _, err := conn.Write(writeBuf); err != nil {
			log.WithError(err).Errorf("%v: error while writing", connType)
			break
		}
		log.Infof("%v: wrote %v bytes", connType, n)
	}
}

func (self *echoServer) HandleCustomAgentAsyncOp(conn net.Conn) error {
	logrus.Debug("received agent operation request")

	appIdBuf := []byte{0}
	_, err := io.ReadFull(conn, appIdBuf)
	if err != nil {
		return err
	}
	appId := appIdBuf[0]

	if appId != EchoServerAppId {
		logrus.WithField("appId", appId).Debug("invalid app id on agent request")
		return errors.New("invalid operation for controller")
	}

	options := channel.DefaultOptions()
	options.ConnectTimeout = time.Second
	tId := &identity.TokenId{Identity: self.zitiIdentity, Token: "echo-server"}
	listener := channel.NewExistingConnListener(tId, conn, nil)
	_, err = channel.NewChannel("agent", listener, channel.BindHandlerF(self.bindAgentChannel), options)
	return err
}

func (self *echoServer) bindAgentChannel(binding channel.Binding) error {
	binding.AddReceiveHandlerF(EchoServerUpdateTerminator, self.HandleUpdateTerminator)
	return nil
}

func (self *echoServer) HandleUpdateTerminator(msg *channel.Message, ch channel.Channel) {
	log := pfxlog.Logger()
	precedenceLabel, hasPrecedence := msg.GetStringHeader(EchoServerPrecedenceHeader)
	var precedence edge.Precedence

	if hasPrecedence {
		if strings.EqualFold("default", precedenceLabel) {
			precedence = edge.PrecedenceDefault
		} else if strings.EqualFold("required", precedenceLabel) {
			precedence = edge.PrecedenceRequired
		} else if strings.EqualFold("failed", precedenceLabel) {
			precedence = edge.PrecedenceFailed
		} else {
			err := errors.Errorf("invalid precedence [%v]", precedenceLabel)
			self.SendOpResult(msg, ch, "update-precedence", err.Error(), false)
			return
		}
	}

	cost, hasCost := msg.GetUint16Header(EchoServerCostHeader)

	var err error
	if hasPrecedence && hasCost {
		log.Infof("updating precedence=%v, cost=%v", precedenceLabel, cost)
		err = self.zitiListener.UpdateCostAndPrecedence(cost, precedence)
	} else if hasPrecedence {
		log.Infof("updating precedence=%v", precedenceLabel)

		err = self.zitiListener.UpdatePrecedence(precedence)
	} else if hasCost {
		log.Infof("updating cost=%v", cost)
		err = self.zitiListener.UpdateCost(cost)
	} else {
		err = errors.New("neither precedence nor cost provided, nothing to do")
	}

	if err == nil {
		self.SendOpResult(msg, ch, "update-precedence", "", true)
	} else {
		self.SendOpResult(msg, ch, "update-precedence", err.Error(), false)
	}
}

func (self *echoServer) SendOpResult(request *channel.Message, ch channel.Channel, op string, message string, success bool) {
	log := pfxlog.ContextLogger(ch.Label()).WithField("operation", op)
	if !success {
		log.Errorf("%v error performing %v: (%s)", ch.LogicalName(), op, message)
	}

	response := channel.NewResult(success, message)
	response.ReplyTo(request)
	if err := response.WithTimeout(time.Second).SendAndWaitForWire(ch); err != nil {
		log.WithError(err).Error("failed to send result")
	}
}

func (self *echoServer) handleResult(err error, bconn *bufio.ReadWriter) error {
	if err != nil {
		return err
	}
	if _, err = bconn.WriteString("OK"); err != nil {
		return err
	}
	return bconn.Flush()
}
