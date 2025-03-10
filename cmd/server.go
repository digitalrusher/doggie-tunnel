package cmd

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"net"
	"sync"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "启动内网穿透服务端",
	Run: func(cmd *cobra.Command, args []string) {
		startPort := viper.GetInt("server.start_port")
		endPort := viper.GetInt("server.end_port")

		portPool := NewPortPool(startPort, endPort)
		listener, err := net.Listen("tcp", fmt.Sprintf(":%d", viper.GetInt("server.port")))
		if err != nil {
			logger.Fatal("服务端启动失败", zap.Error(err))
		}

		logger.Info("服务端已启动",
			zap.Int("control_port", viper.GetInt("server.port")),
			zap.Int("min_port", startPort),
			zap.Int("max_port", endPort))

		for {
			conn, err := listener.Accept()
			if err != nil {
				logger.Error("接受连接失败", zap.Error(err))
				continue
			}
			go handleClient(conn, portPool)
		}
	},
}

type PortPool struct {
	mu      sync.Mutex
	ports   map[int]bool
	minPort int
	maxPort int
}

func NewPortPool(min, max int) *PortPool {
	p := &PortPool{
		ports:   make(map[int]bool),
		minPort: min,
		maxPort: max,
	}
	for i := min; i <= max; i++ {
		p.ports[i] = true
	}
	return p
}

func (p *PortPool) GetRandomPort() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.ports) == 0 {
		return 0, fmt.Errorf("no available ports")
	}

	n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(p.ports))))
	idx := int(n.Int64())

	var port int
	cnt := 0
	for p := range p.ports {
		if cnt == idx {
			port = p
			break
		}
		cnt++
	}
	delete(p.ports, port)
	return port, nil
}

func handleClient(conn net.Conn, portPool *PortPool) {
	defer conn.Close()

	port, err := portPool.GetRandomPort()
	if err != nil {
		logger.Error("获取端口失败", zap.Error(err))
		return
	}

	logger.Info("分配隧道端口",
		zap.String("client", conn.RemoteAddr().String()),
		zap.Int("port", port))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 隧道转发逻辑
	forwarder := func(src, dst net.Conn) {
		defer src.Close()
		defer dst.Close()
		for {
			select {
			case <-ctx.Done():
				return
			default:
				buf := make([]byte, 1024)
				n, err := src.Read(buf)
				if err != nil {
					return
				}
				_, err = dst.Write(buf[:n])
				if err != nil {
					return
				}
			}
		}
	}

	// 等待客户端连接隧道端口
	tunnelListener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		logger.Error("隧道端口监听失败", zap.Error(err))
		return
	}
	defer tunnelListener.Close()

	conn.Write([]byte(fmt.Sprintf("PORT:%d\n", port)))

	tunnelConn, err := tunnelListener.Accept()
	if err != nil {
		logger.Error("接受隧道连接失败", zap.Error(err))
		return
	}
	defer tunnelConn.Close()

	go forwarder(conn, tunnelConn)
	forwarder(tunnelConn, conn)
}

func init() {
	rootCmd.AddCommand(serverCmd)

	serverCmd.Flags().IntP("port", "p", 8080, "服务端控制端口")
	serverCmd.Flags().Int("start", 10000, "端口池起始端口")
	serverCmd.Flags().Int("end", 20000, "端口池结束端口")

	viper.BindPFlag("server.port", serverCmd.Flags().Lookup("port"))
	viper.BindPFlag("server.start_port", serverCmd.Flags().Lookup("start"))
	viper.BindPFlag("server.end_port", serverCmd.Flags().Lookup("end"))
}
