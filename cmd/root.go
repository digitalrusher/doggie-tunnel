package cmd

import (
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

var logger *zap.Logger

var rootCmd = &cobra.Command{
	Use:   "doggie-tunnel",
	Short: "TCP隧道内网穿透系统",
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		logger, _ = zap.NewProduction()
		viper.AddConfigPath(".")
		viper.SetConfigFile("tunnel.yaml")
		viper.ReadInConfig()
	},
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		logger.Fatal("命令执行错误", zap.Error(err))
	}
}
