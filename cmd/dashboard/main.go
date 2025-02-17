package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"time"

	"github.com/ory/graceful"
	"github.com/patrickmn/go-cache"
	"github.com/robfig/cron/v3"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/naiba/nezha/cmd/dashboard/controller"
	"github.com/naiba/nezha/cmd/dashboard/rpc"
	"github.com/naiba/nezha/model"
	"github.com/naiba/nezha/service/singleton"
)

func init() {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		panic(err)
	}

	// 初始化 dao 包
	singleton.Init()
	singleton.Conf = &model.Config{}
	singleton.Cron = cron.New(cron.WithSeconds(), cron.WithLocation(shanghai))
	singleton.Crons = make(map[uint64]*model.Cron)
	singleton.ServerList = make(map[uint64]*model.Server)
	singleton.SecretToID = make(map[string]uint64)

	err = singleton.Conf.Read("data/config.yaml")
	if err != nil {
		panic(err)
	}
	singleton.DB, err = gorm.Open(sqlite.Open("data/sqlite.db"), &gorm.Config{
		CreateBatchSize: 200,
	})
	if err != nil {
		panic(err)
	}
	if singleton.Conf.Debug {
		singleton.DB = singleton.DB.Debug()
	}
	if singleton.Conf.GRPCPort == 0 {
		singleton.Conf.GRPCPort = 5555
	}
	singleton.Cache = cache.New(5*time.Minute, 10*time.Minute)

	initSystem()
}

func initSystem() {
	singleton.DB.AutoMigrate(model.Server{}, model.User{},
		model.Notification{}, model.AlertRule{}, model.Monitor{},
		model.MonitorHistory{}, model.Cron{}, model.Transfer{})

	singleton.LoadNotifications()
	loadServers() //加载服务器列表
	loadCrons()   //加载计划任务

	// 清理 服务请求记录 和 流量记录 的旧数据
	_, err := singleton.Cron.AddFunc("0 30 3 * * *", cleanMonitorHistory)
	if err != nil {
		panic(err)
	}

	// 流量记录打点
	_, err = singleton.Cron.AddFunc("0 0 * * * *", recordTransferHourlyUsage)
	if err != nil {
		panic(err)
	}
}

func recordTransferHourlyUsage() {
	singleton.ServerLock.Lock()
	defer singleton.ServerLock.Unlock()
	now := time.Now()
	nowTrimSeconds := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, time.Local)
	var txs []model.Transfer
	for id, server := range singleton.ServerList {
		tx := model.Transfer{
			ServerID: id,
			In:       server.State.NetInTransfer - uint64(server.PrevHourlyTransferIn),
			Out:      server.State.NetOutTransfer - uint64(server.PrevHourlyTransferOut),
		}
		if tx.In == 0 && tx.Out == 0 {
			continue
		}
		server.PrevHourlyTransferIn = int64(server.State.NetInTransfer)
		server.PrevHourlyTransferOut = int64(server.State.NetOutTransfer)
		tx.CreatedAt = nowTrimSeconds
		txs = append(txs, tx)
	}
	if len(txs) == 0 {
		return
	}
	log.Println("NEZHA>> Cron 流量统计入库", len(txs), singleton.DB.Create(txs).Error)
}

func cleanMonitorHistory() {
	// 清理无效数据
	singleton.DB.Unscoped().Delete(&model.MonitorHistory{}, "created_at < ? OR monitor_id NOT IN (SELECT `id` FROM monitors)", time.Now().AddDate(0, 0, -30))
	singleton.DB.Unscoped().Delete(&model.Transfer{}, "server_id NOT IN (SELECT `id` FROM servers)")
	// 计算可清理流量记录的时长
	var allServerKeep time.Time
	specialServerKeep := make(map[uint64]time.Time)
	var specialServerIDs []uint64
	var alerts []model.AlertRule
	singleton.DB.Find(&alerts)
	for i := 0; i < len(alerts); i++ {
		for j := 0; j < len(alerts[i].Rules); j++ {
			// 是不是流量记录规则
			if !alerts[i].Rules[j].IsTransferDurationRule() {
				continue
			}
			dataCouldRemoveBefore := alerts[i].Rules[j].GetTransferDurationStart()
			// 判断规则影响的机器范围
			if alerts[i].Rules[j].Cover == model.RuleCoverAll {
				// 更新全局可以清理的数据点
				if allServerKeep.IsZero() || allServerKeep.After(dataCouldRemoveBefore) {
					allServerKeep = dataCouldRemoveBefore
				}
			} else {
				// 更新特定机器可以清理数据点
				for id := range alerts[i].Rules[j].Ignore {
					if specialServerKeep[id].IsZero() || specialServerKeep[id].After(dataCouldRemoveBefore) {
						specialServerKeep[id] = dataCouldRemoveBefore
						specialServerIDs = append(specialServerIDs, id)
					}
				}
			}
		}
	}
	for id, couldRemove := range specialServerKeep {
		singleton.DB.Unscoped().Delete(&model.Transfer{}, "server_id = ? AND created_at < ?", id, couldRemove)
	}
	if allServerKeep.IsZero() {
		singleton.DB.Unscoped().Delete(&model.Transfer{}, "server_id NOT IN (?)", specialServerIDs)
	} else {
		singleton.DB.Unscoped().Delete(&model.Transfer{}, "server_id NOT IN (?) AND created_at < ?", specialServerIDs, allServerKeep)
	}
}

func loadServers() {
	var servers []model.Server
	singleton.DB.Find(&servers)
	for _, s := range servers {
		innerS := s
		innerS.Host = &model.Host{}
		innerS.State = &model.HostState{}
		singleton.ServerList[innerS.ID] = &innerS
		singleton.SecretToID[innerS.Secret] = innerS.ID
	}
	singleton.ReSortServer()
}

func loadCrons() {
	var crons []model.Cron
	singleton.DB.Find(&crons)
	var err error
	errMsg := new(bytes.Buffer)
	for i := 0; i < len(crons); i++ {
		cr := crons[i]

		crIgnoreMap := make(map[uint64]bool)
		for j := 0; j < len(cr.Servers); j++ {
			crIgnoreMap[cr.Servers[j]] = true
		}

		cr.CronJobID, err = singleton.Cron.AddFunc(cr.Scheduler, singleton.CronTrigger(cr))
		if err == nil {
			singleton.Crons[cr.ID] = &cr
		} else {
			if errMsg.Len() == 0 {
				errMsg.WriteString("调度失败的计划任务：[")
			}
			errMsg.WriteString(fmt.Sprintf("%d,", cr.ID))
		}
	}
	if errMsg.Len() > 0 {
		msg := errMsg.String()
		singleton.SendNotification(msg[:len(msg)-1]+"] 这些任务将无法正常执行,请进入后点重新修改保存。", false)
	}
	singleton.Cron.Start()
}

func main() {
	cleanMonitorHistory()
	go rpc.ServeRPC(singleton.Conf.GRPCPort)
	serviceSentinelDispatchBus := make(chan model.Monitor)
	go rpc.DispatchTask(serviceSentinelDispatchBus)
	go rpc.DispatchKeepalive()
	go singleton.AlertSentinelStart()
	singleton.NewServiceSentinel(serviceSentinelDispatchBus)
	srv := controller.ServeWeb(singleton.Conf.HTTPPort)
	graceful.Graceful(func() error {
		return srv.ListenAndServe()
	}, func(c context.Context) error {
		log.Println("NEZHA>> Graceful::START")
		recordTransferHourlyUsage()
		log.Println("NEZHA>> Graceful::END")
		srv.Shutdown(c)
		return nil
	})
}
