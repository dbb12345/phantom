package proxy

import (
	"fmt"
	"math/rand"
	"net"
	"time"

	"github.com/jhead/phantom/internal/clientmap"
	"github.com/jhead/phantom/internal/proto"
	"github.com/rs/zerolog/log"
	"github.com/tevino/abool"

	reuse "github.com/libp2p/go-reuseport"
)

const maxMTU = 1472

var idleCheckInterval = 5 * time.Second

type ProxyServer struct {
	bindAddress         *net.UDPAddr
	boundPort           uint16
	remoteServerAddress *net.UDPAddr
	pingServer          net.PacketConn
	pingServerV6        net.PacketConn
	server              *net.UDPConn
	clientMap           *clientmap.ClientMap
	prefs               ProxyPrefs
	dead                *abool.AtomicBool
}

type ProxyPrefs struct {
	BindAddress  string
	BindPort     uint16
	RemoteServer string
	IdleTimeout  time.Duration
	EnableIPv6   bool
	RemovePorts  bool
}

var randSource = rand.NewSource(time.Now().UnixNano())
var serverID = randSource.Int63()

func New(prefs ProxyPrefs) (*ProxyServer, error) {
	bindPort := prefs.BindPort

	// Randomize port if not provided
	if bindPort == 0 {
		bindPort = (uint16(randSource.Int63()) % 14000) + 50000
	}

	// Format full bind address with port
	prefs.BindAddress = fmt.Sprintf("%s:%d", prefs.BindAddress, bindPort)

	bindAddress, err := net.ResolveUDPAddr("udp", prefs.BindAddress)
	if err != nil {
		return nil, fmt.Errorf("Invalid bind address: %s", err)
	}

	remoteServerAddress, err := net.ResolveUDPAddr("udp", prefs.RemoteServer)
	if err != nil {
		return nil, fmt.Errorf("Invalid server address: %s", err)
	}

	return &ProxyServer{
		bindAddress,
		bindPort,
		remoteServerAddress,
		nil,
		nil,
		nil,
		clientmap.New(prefs.IdleTimeout, idleCheckInterval),
		prefs,
		abool.New(),
	}, nil
}

func (proxy *ProxyServer) Start() error {
	// Bind to 19132 on all addresses to receive broadcasted pings
	// Sets SO_REUSEADDR et al to support multiple instances of phantom
	log.Info().Msgf("Binding ping server to port 19132")
	if pingServer, err := reuse.ListenPacket("udp4", ":19132"); err == nil {
		proxy.pingServer = pingServer

		// Start proxying ping packets from the broadcast listener
		go proxy.readLoop(proxy.pingServer)
	} else {
		// Bind failed
		return err
	}

	// Minecraft automatically broadcasts on port 19133 to the local IPv6 network
	if proxy.prefs.EnableIPv6 {
		log.Info().Msgf("Binding IPv6 ping server to port 19133")
		if pingServerV6, err := reuse.ListenPacket("udp6", ":19133"); err == nil {
			proxy.pingServerV6 = pingServerV6

			// Start proxying ping packets from the broadcast listener
			go proxy.readLoop(proxy.pingServerV6)
		} else {
			// IPv6 Bind failed
			log.Warn().Msgf("Failed to bind IPv6 ping listener: %v", err)
		}
	}

	network := "udp4"
	if proxy.prefs.EnableIPv6 {
		network = "udp"
	}

	// Bind to specified UDP addr and port to receive data from Minecraft clients
	log.Info().Msgf("Binding proxy server to: %v", proxy.bindAddress)
	if server, err := net.ListenUDP(network, proxy.bindAddress); err == nil {
		proxy.server = server
	} else {
		return err
	}

	log.Info().Msgf("Proxy server listening!")

	// Start processing everything else using the proxy listener
	proxy.readLoop(proxy.server)

	return nil
}

func (proxy *ProxyServer) Close() {
	log.Info().Msgf("Stopping proxy server")

	// Stop UDP listeners
	proxy.server.Close()
	proxy.pingServer.Close()

	if proxy.pingServerV6 != nil {
		proxy.pingServerV6.Close()
	}

	// Close all connections
	proxy.clientMap.Close()

	// Stop loops
	proxy.dead.Set()
}

// Continually reads data from the provided listener and passes it to
// processDataFromClients until the ProxyServer has been closed.
func (proxy *ProxyServer) readLoop(listener net.PacketConn) {
	packetBuffer := make([]byte, maxMTU)

	for !proxy.dead.IsSet() {
		err := proxy.processDataFromClients(listener, packetBuffer)
		if err != nil {
			log.Info().Msgf("Error while processing client data: %s", err)
		}
	}

	log.Info().Msgf("Listener shut down: %s", listener.LocalAddr())
}

// Inspects an incoming UDP packet, looking up the client in our connection
// map, lazily creating a new connection to the remote server when necessary,
// then forwarding the data to that remote connection.
//
// When a new client connects, an additional goroutine is created to read
// data from the server and send it back to the client.
func (proxy *ProxyServer) processDataFromClients(listener net.PacketConn, packetBuffer []byte) error {
	read, client, _ := listener.ReadFrom(packetBuffer)
	if read <= 0 {
		return nil
	}

	data := packetBuffer[:read]
	log.Trace().Msgf("client recv: %v", data)

	// Handler triggered when a new client connects and we create a new connetion to the remote server
	onNewConnection := func(newServerConn *net.UDPConn) {
		log.Info().Msgf("New connection from client %s -> %s", client.String(), listener.LocalAddr())
		proxy.processDataFromServer(newServerConn, client)
	}

	serverConn, err := proxy.clientMap.Get(
		client,
		proxy.remoteServerAddress,
		onNewConnection,
	)

	if err != nil {
		return err
	}

	// Write packet from client to server
	_, err = serverConn.Write(data)
	return err
}

// Proxies packets sent by the server to us for a specific Minecraft client back to
// that client's UDP connection.
func (proxy *ProxyServer) processDataFromServer(remoteConn *net.UDPConn, client net.Addr) {
	buffer := make([]byte, maxMTU)

	for !proxy.dead.IsSet() {
		read, _, err := remoteConn.ReadFrom(buffer)

		// Read error
		if err != nil {
			log.Warn().Msgf("%v", err)
			break
		}

		// Empty read
		if read < 1 {
			continue
		}

		// Resize data to byte count from 'read'
		data := buffer[:read]
		log.Trace().Msgf("server recv: %v", data)

		// Rewrite Unconnected Reply packets
		if packetID := data[0]; packetID == proto.UnconnectedReplyID {
			log.Debug().Msgf("Received Unconnected Pong from server: %v", data)

			if packet, err := proto.ReadUnconnectedReply(data); err == nil {
				// Overwrite the server ID with one unique to this phantom instance.
				// If we don't do this, the client will get confused if you restart phantom.
				packet.Pong.ServerID = fmt.Sprintf("%d", serverID)

				// Overwrite port numbers sent back from server (if any)
				if packet.Pong.Port4 != "" && !proxy.prefs.RemovePorts {
					packet.Pong.Port4 = fmt.Sprintf("%d", proxy.boundPort)
					packet.Pong.Port6 = packet.Pong.Port4
				} else if proxy.prefs.RemovePorts {
					packet.Pong.Port4 = ""
					packet.Pong.Port6 = ""
				}

				// Rewrite server MOTD to remove ports
				packetBuffer := packet.Build()
				data = packetBuffer.Bytes()

				log.Debug().Msgf("Sent Unconnected Pong to client: %v", packet)
			} else {
				log.Warn().Msgf("Failed to rewrite pong: %v", err)
			}
		}

		proxy.server.WriteTo(data, client)
	}
}
