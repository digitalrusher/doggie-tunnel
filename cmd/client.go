package cmd

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

var clientCmd = &cobra.Command{
	Use:   "client",
	Short: "启动内网穿透客户端",
	Run: func(cmd *cobra.Command, args []string) {
		serverAddr := viper.GetString("client.server_addr")
		localPort := viper.GetInt("client.local_port")
		remotePort := viper.GetInt("client.remote_port")

		logger.Info("客户端启动中",
			zap.String("server", serverAddr),
			zap.Int("local_port", localPort),
			zap.Int("remote_port", remotePort))

		for {
			conn, err := net.Dial("tcp", serverAddr)
			if err != nil {
				logger.Error("连接服务端失败", zap.Error(err))
				time.Sleep(5 * time.Second)
				continue
			}

			logger.Info("成功连接服务端")
			ctx, cancel := context.WithCancel(context.Background())

			// 心跳维护
			go heartbeat(ctx, conn, 10*time.Second)

			// 隧道建立
			go func() {
				defer cancel()
				if err := createTunnel(ctx, conn, localPort); err != nil {
					logger.Error("隧道建立失败", zap.Error(err))
				}
			}()

			// 连接监控
			select {
			case <-ctx.Done():
				conn.Close()
				logger.Info("连接生命周期结束，启动重连")
			}
		}
	},
}

// 新增心跳检测函数
// heartbeat 连接心跳维护机制
// 参数:
//   ctx - 上下文控制
//   conn - 目标连接
//   interval - 心跳间隔时间
// 功能:
//   1. 定期发送心跳包保持连接活跃
//   2. 监测上下文关闭事件
//   3. 记录心跳异常事件
func heartbeat(ctx context.Context, conn net.Conn, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	logger.Debug("启动心跳检测",
		zap.String("remote", conn.RemoteAddr().String()),
		zap.Duration("interval", interval))

	for {
		select {
		case <-ticker.C:
			if _, err := conn.Write([]byte("PING\n")); err != nil {
				logger.Warn("心跳发送失败",
					zap.String("remote", conn.RemoteAddr().String()),
					zap.Error(err))
				return
			}
			logger.Debug("心跳包已发送",
				zap.String("remote", conn.RemoteAddr().String()))

		case <-ctx.Done():
			logger.Debug("上下文关闭，终止心跳检测",
				zap.String("remote", conn.RemoteAddr().String()))
			return
		}
	}
}

// 新增隧道创建函数
func createTunnel(ctx context.Context, ctrlConn net.Conn, localPort int) error {
	defer ctrlConn.Close()

	buf := make([]byte, 1024)
	n, err := ctrlConn.Read(buf)
	if err != nil {
		return fmt.Errorf("读取服务端响应失败: %w", err)
	}

	var remotePort int
	if _, err := fmt.Sscanf(string(buf[:n]), "PORT:%d", &remotePort); err != nil {
		return fmt.Errorf("解析端口失败: %w", err)
	}

	logger.Info("隧道端口已分配",
		zap.Int("local", localPort),
		zap.Int("remote", remotePort))

	// 启动本地监听和端口转发
	return startPortForwarding(ctx, ctrlConn, localPort, remotePort)
}

// 新增数据转发函数
// forward 双向流量转发核心逻辑
// 参数:
//   src - 源端连接
//   dst - 目标端连接
// 功能:
//   1. 建立带超时控制的转发通道
//   2. 实时统计传输字节数
//   3. 监控连接中断事件
func forward(src, dst net.Conn) {
	defer src.Close()
	defer dst.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger.Info("启动流量转发通道",
		zap.String("src", src.RemoteAddr().String()),
		zap.String("dst", dst.RemoteAddr().String()))

	for {
		select {
		case <-ctx.Done():
			logger.Debug("上下文关闭，终止转发循环")
			return
		default:
			dst.SetWriteDeadline(time.Now().Add(30 * time.Second))
			startTime := time.Now()
			written, err := io.Copy(dst, src)
			
			if err != nil {
				logger.Warn("流量转发中断",
					zap.Error(err),
					zap.String("direction", fmt.Sprintf("%s -> %s", 
						src.RemoteAddr().String(), dst.RemoteAddr().String())),
					zap.Duration("duration", time.Since(startTime)))
				return
			}
			
			logger.Debug("流量中继完成",
				zap.Int64("bytes", written),
				zap.String("from", src.RemoteAddr().String()),
				zap.String("to", dst.RemoteAddr().String()),
				zap.Duration("duration", time.Since(startTime)),
				zap.Time("start_time", startTime))
		}
	}
}

// 新增连接重试逻辑
func connectWithRetry(addr string, maxRetries int) (net.Conn, error) {
	var conn net.Conn
	var err error
	for i := 0; i < maxRetries; i++ {
		conn, err = net.Dial("tcp", addr)
		if err == nil {
			return conn, nil
		}
		time.Sleep(time.Second * time.Duration(i+1))
	}
	return nil, err
}

func init() {
	rootCmd.AddCommand(clientCmd)

	clientCmd.Flags().StringP("server", "s", "127.0.0.1:8080", "服务端地址")
	clientCmd.Flags().IntP("local", "l", 80, "本地服务端口")
	clientCmd.Flags().IntP("remote", "r", 0, "远程绑定端口（0表示自动分配）")

	viper.BindPFlag("client.server_addr", clientCmd.Flags().Lookup("server"))
	viper.BindPFlag("client.local_port", clientCmd.Flags().Lookup("local"))
	viper.BindPFlag("client.remote_port", clientCmd.Flags().Lookup("remote"))
}

// 新增连接管理函数
func manageConnection(serverAddr string, localPort int) {
	for {
		ctrlConn, err := connectWithRetry(serverAddr, 5)
		if err != nil {
			logger.Error("控制连接重建失败", zap.Error(err))
			time.Sleep(5 * time.Second)
			continue
		}

		ctx, cancel := context.WithCancel(context.Background())

		go heartbeat(ctx, ctrlConn, 10*time.Second)
		go func() {
			defer cancel()
			if err := createTunnel(ctx, ctrlConn, localPort); err != nil {
				logger.Error("隧道建立失败", zap.Error(err))
			}
		}()

		// 连接状态监控
		connCheckTicker := time.NewTicker(5 * time.Second)
		defer connCheckTicker.Stop()

		for {
			select {
			case <-connCheckTicker.C:
				if _, err := ctrlConn.Write([]byte{}); err != nil {
					logger.Warn("控制连接异常，准备重连")
					ctrlConn.Close()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}
}

func startPortForwarding(ctx context.Context, ctrlConn net.Conn, localPort int, remotePort int) error {
	localAddr := fmt.Sprintf(":%d", localPort)
	listener, err := net.Listen("tcp", localAddr)
	if err != nil {
		return fmt.Errorf("本地监听失败: %w", err)
	}
	defer listener.Close()

	logger.Info("本地服务已启动", zap.Int("port", localPort))

	go func() {
		<-ctx.Done()
		listener.Close()
		logger.Info("本地监听器已关闭")
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			localConn, err := listener.Accept()
			if err != nil {
				logger.Warn("接受本地连接失败", zap.Error(err))
				continue
			}

			remoteConn, err := connectWithRetry(ctrlConn.RemoteAddr().String(), 3)
			if err != nil {
				localConn.Close()
				logger.Error("连接远程隧道失败", zap.Error(err))
				continue
			}

			logger.Info("建立端口转发通道",
				zap.String("local", localConn.RemoteAddr().String()),
				zap.String("remote", remoteConn.RemoteAddr().String()))

			go forward(localConn, remoteConn)
			go forward(remoteConn, localConn)
		}
	}
}
