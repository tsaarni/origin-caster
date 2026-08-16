package dial

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grandcat/zeroconf"
)

// AppHandlerFunc is called when a DIAL launch request is received.
type AppHandlerFunc func(appName, payload string) error

// Server implements the DIAL (Discovery and Launch) protocol and SSDP responder.
type Server struct {
	friendlyName string
	uuid         string
	localIP      net.IP
	port         int
	httpServer   *http.Server
	mdnsServer   *zeroconf.Server
	ssdpConn     *net.UDPConn
	appHandler   AppHandlerFunc
	mu           sync.RWMutex
	activeApps   map[string]string // appName -> state ("running", "stopped")
	closed       int32
}

// NewServer creates a new DIAL protocol server.
func NewServer(friendlyName, uuid string, localIP net.IP, port int, handler AppHandlerFunc) *Server {
	return &Server{
		friendlyName: friendlyName,
		uuid:         uuid,
		localIP:      localIP,
		port:         port,
		appHandler:   handler,
		activeApps:   make(map[string]string),
	}
}

// Start begins listening for DIAL HTTP requests and SSDP discovery.
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ssdp/device-desc.xml", s.handleDeviceDesc)
	mux.HandleFunc("/apps/", s.handleApps)

	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: mux,
	}

	// 1. Start HTTP Server
	go func() {
		log.Printf("[DIAL] HTTP service listening on http://%s:%d/ssdp/device-desc.xml", s.localIP.String(), s.port)
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[DIAL] HTTP server error: %v", err)
		}
	}()

	// 2. Start mDNS _dial._tcp advertisement
	instanceName := strings.ReplaceAll(s.friendlyName, " ", "-")
	mdnsServer, err := zeroconf.Register(instanceName, "_dial._tcp", "local.", s.port, []string{"v=1"}, nil)
	if err == nil {
		s.mdnsServer = mdnsServer
		log.Printf("[DIAL] Advertising _dial._tcp on port %d", s.port)
	}

	// 3. Start SSDP Multicast Listener on 239.255.255.250:1900
	go s.startSSDP()

	return nil
}

// Close stops the DIAL server.
func (s *Server) Close() {
	if atomic.CompareAndSwapInt32(&s.closed, 0, 1) {
		if s.httpServer != nil {
			_ = s.httpServer.Close()
		}
		if s.mdnsServer != nil {
			s.mdnsServer.Shutdown()
		}
		if s.ssdpConn != nil {
			_ = s.ssdpConn.Close()
		}
	}
}

func (s *Server) handleDeviceDesc(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Application-URL", fmt.Sprintf("http://%s:%d/apps/", s.localIP.String(), s.port))

	xml := fmt.Sprintf(`<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <specVersion>
    <major>1</major>
    <minor>0</minor>
  </specVersion>
  <device>
    <deviceType>urn:dial-multiscreen-org:device:dial:1</deviceType>
    <friendlyName>%s</friendlyName>
    <manufacturer>Google Inc.</manufacturer>
    <modelName>Chromecast</modelName>
    <UDN>uuid:%s</UDN>
    <serviceList>
      <service>
        <serviceType>urn:dial-multiscreen-org:service:dial:1</serviceType>
        <serviceId>urn:dial-multiscreen-org:serviceId:dial</serviceId>
        <controlURL>/ssdp/notused</controlURL>
        <eventSubURL>/ssdp/notused</eventSubURL>
        <SCPDURL>/ssdp/notused</SCPDURL>
      </service>
    </serviceList>
  </device>
</root>`, s.friendlyName, s.uuid)

	_, _ = w.Write([]byte(xml))
}

func (s *Server) handleApps(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/apps/")
	parts := strings.Split(path, "/")
	appName := parts[0]

	if appName == "" {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/xml")
		s.mu.RLock()
		state := s.activeApps[appName]
		s.mu.RUnlock()
		if state == "" {
			state = "stopped"
		}
		resp := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<service xmlns="urn:dial-multiscreen-org:schemas:dial" version="1.0">
  <name>%s</name>
  <options allowStop="true"/>
  <state>%s</state>
</service>`, appName, state)
		_, _ = w.Write([]byte(resp))

	case http.MethodPost:
		log.Printf("[DIAL] Received launch request for app: %s", appName)
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		payload := string(buf[:n])

		s.mu.Lock()
		s.activeApps[appName] = "running"
		s.mu.Unlock()

		if s.appHandler != nil {
			go func() {
				if err := s.appHandler(appName, payload); err != nil {
					log.Printf("[DIAL] App handler error for %s: %v", appName, err)
				}
			}()
		}

		w.Header().Set("Location", fmt.Sprintf("http://%s:%d/apps/%s/run", s.localIP.String(), s.port, appName))
		w.WriteHeader(http.StatusCreated)

	case http.MethodDelete:
		s.mu.Lock()
		delete(s.activeApps, appName)
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) startSSDP() {
	addr, err := net.ResolveUDPAddr("udp4", "239.255.255.250:1900")
	if err != nil {
		return
	}

	conn, err := net.ListenMulticastUDP("udp4", nil, addr)
	if err != nil {
		return
	}
	s.ssdpConn = conn

	buf := make([]byte, 2048)
	for {
		if atomic.LoadInt32(&s.closed) == 1 {
			return
		}
		n, src, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}
		req := string(buf[:n])
		if strings.Contains(req, "M-SEARCH") && (strings.Contains(req, "urn:dial-multiscreen-org:service:dial:1") || strings.Contains(req, "ssdp:all")) {
			resp := fmt.Sprintf("HTTP/1.1 200 OK\r\n"+
				"CACHE-CONTROL: max-age=1800\r\n"+
				"DATE: %s\r\n"+
				"EXT:\r\n"+
				"LOCATION: http://%s:%d/ssdp/device-desc.xml\r\n"+
				"SERVER: Linux/3.14.0 UPnP/1.0 GUPnP/0.20.14\r\n"+
				"ST: urn:dial-multiscreen-org:service:dial:1\r\n"+
				"USN: uuid:%s::urn:dial-multiscreen-org:service:dial:1\r\n"+
				"BOOTID.UPNP.ORG: 1\r\n"+
				"CONFIGID.UPNP.ORG: 1\r\n\r\n",
				time.Now().Format(time.RFC1123), s.localIP.String(), s.port, s.uuid)
			uConn, err := net.Dial("udp4", src.String())
			if err == nil {
				_, _ = uConn.Write([]byte(resp))
				_ = uConn.Close()
			}
		}
	}
}
