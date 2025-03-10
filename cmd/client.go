package cmd

import (
	"fmt"
	"io"
	"net"
	"strings"
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

		// 新增连接逻辑
		var retries int
		maxRetries := 5
		backoff := time.Second

		for {
			conn, err := net.Dial("tcp", serverAddr)
			if err != nil {
				logger.Error("连接服务端失败", zap.Error(err))
				if retries >= maxRetries {
					logger.Fatal("超过最大重试次数")
				}

				time.Sleep(backoff)
				backoff *= 2
				retries++
				continue
			}

			logger.Info("成功连接服务端")
			go heartbeat(conn, 10*time.Second)
			go createTunnel(conn, localPort)
			break
		}
	},
}

// 新增心跳检测函数
func heartbeat(conn net.Conn, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			_, err := conn.Write([]byte("PING\n"))
			if err != nil {
				logger.Error("心跳发送失败", zap.Error(err))
				conn.Close()
				return
			}
		}
	}
}

// 新增隧道创建函数
func createTunnel(ctrlConn net.Conn, localPort int) {
	defer ctrlConn.Close()

	// 读取服务端分配的端口
	buf := make([]byte, 1024)
	n, err := ctrlConn.Read(buf)
	if err != nil {
		logger.Error("读取服务端响应失败", zap.Error(err))
		return
	}

	resp := string(buf[:n])
	var remotePort int
	_, err = fmt.Sscanf(resp, "PORT:%d", &remotePort)
	if err != nil {
		logger.Error("解析端口失败", zap.Error(err))
		return
	}

	logger.Info("隧道建立成功",
		zap.Int("local_port", localPort),
		zap.Int("remote_port", remotePort))

	// 启动本地端口监听
	localListener, err := net.Listen("tcp", fmt.Sprintf(":%d", localPort))
	if err != nil {
		logger.Error("本地端口监听失败", zap.Error(err))
		return
	}
	defer localListener.Close()

	for {
		localConn, err := localListener.Accept()
		if err != nil {
			logger.Error("接受本地连接失败", zap.Error(err))
			continue
		}

		// 连接到服务端隧道端口
		tunnelConn, err := net.Dial("tcp", fmt.Sprintf("%s:%d",
			strings.Split(ctrlConn.RemoteAddr().String(), ":")[0], remotePort))
		if err != nil {
			logger.Error("连接隧道端口失败", zap.Error(err))
			localConn.Close()
			continue
		}

		go forward(localConn, tunnelConn)
		go forward(tunnelConn, localConn)
	}
}

// 新增数据转发函数
func forward(src, dst net.Conn) {
	defer src.Close()
	defer dst.Close()
	io.Copy(dst, src)
}

func init() {
	rootCmd.AddCommand(clientCmd)

	clientCmd.Flags().StringP("server", "s", "127.0.0.1:8080", "服务端地址")
	clientCmd.Flags().IntP("local", "l", 8080, "本地服务端口")
	clientCmd.Flags().IntP("remote", "r", 0, "远程绑定端口（0表示自动分配）")

	viper.BindPFlag("client.server_addr", clientCmd.Flags().Lookup("server"))
	viper.BindPFlag("client.local_port", clientCmd.Flags().Lookup("local"))
	viper.BindPFlag("client.remote_port", clientCmd.Flags().Lookup("remote"))
}
