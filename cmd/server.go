package cmd

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"math/big"
	"net"
	"sync"
	"time"

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

// PortPool 管理可用端口范围
// 字段说明:
// mu - 保证并发安全的互斥锁
// ports - 端口可用状态映射表（true=可用）
// minPort - 端口池起始端口
// maxPort - 端口池结束端口
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

	logger.Info("端口分配完成",
		zap.String("client_ip", conn.RemoteAddr().String()),
		zap.Int("allocated_port", port),
		zap.Int("available_ports", len(portPool.ports)),
		zap.Int("min_port", portPool.minPort),
		zap.Int("max_port", portPool.maxPort))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 隧道转发逻辑（带控制连接绑定）
	forwarder := func(ctrlConn, src, dst net.Conn) {
		defer src.Close()
		defer dst.Close()
	
		// 连接绑定校验
		if _, err := ctrlConn.Write([]byte("BIND_VERIFY")); err != nil {
			logger.Warn("控制连接不可用", zap.Error(err))
			return
		}
	
		// 双向流量监控
		errChan := make(chan error, 2)
		forwardTask := func(src, dst net.Conn) {
			for {
				select {
				case <-ctx.Done():
					return
				default:
					dst.SetWriteDeadline(time.Now().Add(30 * time.Second))
					written, err := io.Copy(dst, src)
					if err != nil {
						errChan <- fmt.Errorf("%s->%s: %w", 
							src.RemoteAddr().String(), dst.RemoteAddr().String(), err)
						return
					}
					logger.Debug("流量转发中继",
						zap.Int64("bytes", written),
						zap.String("from", src.RemoteAddr().String()),
						zap.String("to", dst.RemoteAddr().String()))
				}
			}
		}
	
		go forwardTask(src, dst)
		go forwardTask(dst, src)
	
		// 错误处理流程
		select {
		case err := <-errChan:
			logger.Warn("流量转发中断", zap.Error(err))
			ctrlConn.Close()
		case <-ctx.Done():
		}
	}

	// 等待客户端连接隧道端口
	tunnelListener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		logger.Error("隧道端口监听失败", zap.Error(err))
		return
	}
	defer func() {
		tunnelListener.Close()
		logger.Info("隧道端口监听器已关闭", zap.Int("port", port))
	}()

	conn.Write([]byte(fmt.Sprintf("PORT:%d\n", port)))

	tunnelConn, err := tunnelListener.Accept()
	if err != nil {
		logger.Error("接受隧道连接失败", zap.Error(err))
		return
	}
	defer tunnelConn.Close()

	// 建立双向转发通道（绑定控制连接）
	go forwarder(conn, conn, tunnelConn)
	go forwarder(conn, tunnelConn, conn)

	// 连接关闭后回收端口
	cancel()
	defer func() {
		// 等待所有连接完全关闭
		<-time.After(30*time.Second)
		// 增强型端口回收（立即回收+延迟校验）
		defer func() {
			portPool.mu.Lock()
			portPool.ports[port] = true
			portPool.mu.Unlock()
		
			go func(p int) {
				time.Sleep(35 * time.Second)
				if _, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", p), 2*time.Second); err == nil {
					logger.Warn("端口仍被占用", zap.Int("port", p))
				}
			}(port)
		
			logger.Info("端口已回收", zap.Int("port", port))
		}()
	}()
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
