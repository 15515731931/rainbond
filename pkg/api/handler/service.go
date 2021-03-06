// RAINBOND, Application Management Platform
// Copyright (C) 2014-2017 Goodrain Co., Ltd.

// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version. For any non-GPL usage of Rainbond,
// one or multiple Commercial Licenses authorized by Goodrain Co., Ltd.
// must be obtained first.

// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.

// You should have received a copy of the GNU General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package handler

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/goodrain/rainbond/pkg/db"
	core_model "github.com/goodrain/rainbond/pkg/db/model"
	"github.com/goodrain/rainbond/pkg/event"
	"github.com/goodrain/rainbond/pkg/mq/api/grpc/pb"
	"github.com/twinj/uuid"

	"github.com/jinzhu/gorm"

	"github.com/pquerna/ffjson/ffjson"

	"github.com/goodrain/rainbond/cmd/api/option"
	api_db "github.com/goodrain/rainbond/pkg/api/db"
	api_model "github.com/goodrain/rainbond/pkg/api/model"
	"github.com/goodrain/rainbond/pkg/api/util"
	dbmodel "github.com/goodrain/rainbond/pkg/db/model"
	core_util "github.com/goodrain/rainbond/pkg/util"
	"github.com/goodrain/rainbond/pkg/worker/discover/model"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/Sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
)

//ServiceAction service act
type ServiceAction struct {
	MQClient   pb.TaskQueueClient
	KubeClient *kubernetes.Clientset
}

//CreateManager create Manger
func CreateManager(conf option.Config) (*ServiceAction, error) {
	mq := api_db.MQManager{
		Endpoint: conf.MQAPI,
	}
	mqClient, errMQ := mq.NewMQManager()
	if errMQ != nil {
		logrus.Errorf("new MQ manager failed, %v", errMQ)
		return nil, errMQ
	}
	logrus.Debugf("mqclient is %v", mqClient)
	k8s := api_db.K8SManager{
		K8SConfig: conf.KubeConfig,
	}
	kubeClient, errK := k8s.NewKubeConnection()
	if errK != nil {
		logrus.Errorf("create kubeclient failed, %v", errK)
		return nil, errK
	}
	return &ServiceAction{
		MQClient:   mqClient,
		KubeClient: kubeClient,
	}, nil
}

//ServiceBuild service build
func (s *ServiceAction) ServiceBuild(tenantID, serviceID string, r *api_model.BuildServiceStruct) error {
	eventID := r.Body.EventID
	logger := event.GetManager().GetLogger(eventID)
	defer event.CloseManager()
	service, err := db.GetManager().TenantServiceDao().GetServiceByID(serviceID)
	if err != nil {
		return err
	}
	if r.Body.Kind == "" {
		r.Body.Kind = "source"
	}
	switch r.Body.Kind {
	case "source":
		//源码构建
		if err := s.sourceBuild(r, service); err != nil {
			logger.Error("源码构建应用任务发送失败 "+err.Error(), map[string]string{"step": "callback", "status": "failure"})
			return err
		}
		logger.Info("源码构建应用任务发送成功 ", map[string]string{"step": "source-service", "status": "starting"})
		return nil
	case "slug":
		//源码构建的分享至云市安装回平台
		if err := s.slugBuild(r, service); err != nil {
			logger.Error("slug构建应用任务发送失败"+err.Error(), map[string]string{"step": "callback", "status": "failure"})
			return err
		}
		logger.Info("slug构建应用任务发送成功 ", map[string]string{"step": "source-service", "status": "starting"})
		return nil
	case "image":
		//镜像构建
		if err := s.imageBuild(r, service); err != nil {
			logger.Error("镜像构建应用任务发送失败 "+err.Error(), map[string]string{"step": "callback", "status": "failure"})
			return err
		}
		logger.Info("镜像构建应用任务发送成功 ", map[string]string{"step": "image-service", "status": "starting"})
		return nil
	case "market":
		//镜像构建分享至云市安装回平台
		if err := s.marketBuild(r, service); err != nil {
			logger.Error("云市构建应用任务发送失败 "+err.Error(), map[string]string{"step": "callback", "status": "failure"})
			return err
		}
		logger.Info("云市构建应用任务发送成功 ", map[string]string{"step": "market-service", "status": "starting"})
		return nil
	default:
		return fmt.Errorf("unexpect kind")
	}
}

func (s *ServiceAction) sourceBuild(r *api_model.BuildServiceStruct, service *dbmodel.TenantServices) error {
	logrus.Debugf("build from source")
	if r.Body.RepoURL == "" || r.Body.DeployVersion == "" || r.Body.EventID == "" {
		return fmt.Errorf("args error")
	}
	body := make(map[string]interface{})
	if r.Body.Operator == "" {
		body["operator"] = "system"
	} else {
		body["operator"] = r.Body.Operator
	}
	body["tenant_id"] = service.TenantID
	body["service_id"] = service.ServiceID
	body["repo_url"] = r.Body.RepoURL
	body["action"] = r.Body.Action
	body["deploy_version"] = r.Body.DeployVersion
	body["event_id"] = r.Body.EventID
	body["envs"] = r.Body.ENVS
	body["tenant_name"] = r.Body.TenantName
	body["service_alias"] = r.Body.ServiceAlias
	body["expire"] = 180
	logrus.Debugf("app_build body is %v", body)
	return s.sendTask(body, "app_build")
}

func (s *ServiceAction) imageBuild(r *api_model.BuildServiceStruct, service *dbmodel.TenantServices) error {
	logrus.Debugf("build from image")
	if r.Body.EventID == "" {
		return fmt.Errorf("args error")
	}
	dependIds, err := db.GetManager().TenantServiceRelationDao().GetTenantServiceRelations(service.ServiceID)
	if err != nil {
		return err
	}
	body := make(map[string]interface{})
	if r.Body.Operator == "" {
		body["operator"] = "system"
	} else {
		body["operator"] = r.Body.Operator
	}
	body["image"] = service.ImageName
	body["service_id"] = service.ID
	body["deploy_version"] = r.Body.DeployVersion
	body["app_version"] = service.ServiceVersion
	body["namespace"] = service.Namespace
	body["operator"] = r.Body.Operator
	body["event_id"] = r.Body.EventID
	body["tenant_name"] = r.Body.TenantName
	body["service_alias"] = r.Body.ServiceAlias
	body["action"] = "download_and_deploy"
	body["dep_sids"] = dependIds
	body["code_from"] = "image_manual"
	logrus.Debugf("image_manual body is %v", body)
	return s.sendTask(body, "image_manual")
}

func (s *ServiceAction) slugBuild(r *api_model.BuildServiceStruct, service *dbmodel.TenantServices) error {
	logrus.Debugf("build from slug")
	if r.Body.EventID == "" {
		return fmt.Errorf("args error")
	}
	dependIds, err := db.GetManager().TenantServiceRelationDao().GetTenantServiceRelations(service.ServiceID)
	if err != nil {
		return err
	}
	body := make(map[string]interface{})
	if r.Body.Operator == "" {
		body["operator"] = "system"
	} else {
		body["operator"] = r.Body.Operator
	}
	body["image"] = service.ImageName
	body["service_id"] = service.ID
	body["deploy_version"] = r.Body.DeployVersion
	body["service_alias"] = service.ServiceAlias
	body["app_version"] = service.ServiceVersion
	body["app_key"] = service.ServiceKey
	body["namespace"] = service.Namespace
	body["deploy_version"] = service.DeployVersion
	body["operator"] = r.Body.Operator
	body["event_id"] = r.Body.EventID
	body["tenant_name"] = r.Body.TenantName
	body["service_alias"] = r.Body.ServiceAlias
	body["action"] = "download_and_deploy"
	body["dep_sids"] = dependIds
	logrus.Debugf("image_manual body is %v", body)
	return s.sendTask(body, "app_slug")
}

func (s *ServiceAction) marketBuild(r *api_model.BuildServiceStruct, service *dbmodel.TenantServices) error {
	logrus.Debugf("build from cloud market")
	if r.Body.EventID == "" {
		return fmt.Errorf("args error")
	}
	dependIds, err := db.GetManager().TenantServiceRelationDao().GetTenantServiceRelations(service.ServiceID)
	if err != nil {
		return err
	}
	body := make(map[string]interface{})
	if r.Body.Operator == "" {
		body["operator"] = "system"
	} else {
		body["operator"] = r.Body.Operator
	}
	body["image"] = service.ImageName
	body["service_id"] = service.ID
	body["service_alias"] = service.ServiceAlias
	body["deploy_version"] = r.Body.DeployVersion
	body["app_version"] = service.ServiceVersion
	body["namespace"] = service.Namespace
	body["operator"] = r.Body.Operator
	body["event_id"] = r.Body.EventID
	body["tenant_name"] = r.Body.TenantName
	body["service_alias"] = r.Body.ServiceAlias
	body["action"] = "download_and_deploy"
	body["dep_sids"] = dependIds
	logrus.Debugf("app_image body is %v", body)
	return s.sendTask(body, "app_image")
}

func (s *ServiceAction) sendTask(body map[string]interface{}, taskType string) error {
	bodyJ, err := ffjson.Marshal(body)
	if err != nil {
		return err
	}
	bs := &api_db.BuildTaskStruct{
		TaskType: taskType,
		TaskBody: bodyJ,
		User:     "define",
	}
	eq, errEq := api_db.BuildTaskBuild(bs)
	if errEq != nil {
		logrus.Errorf("build equeue stop request error, %v", errEq)
		return errEq
	}
	_, err = s.MQClient.Enqueue(context.Background(), eq)
	if err != nil {
		logrus.Errorf("equque mq error, %v", err)
		return err
	}
	return nil
}

//AddLabel add labels
func (s *ServiceAction) AddLabel(kind, serviceID string, amp []string) error {
	for _, v := range amp {
		var labelModel dbmodel.TenantServiceLable
		switch kind {
		case "service":
			labelModel.ServiceID = serviceID
			labelModel.LabelKey = core_model.LabelKeyServiceType
			v = chekeServiceLabel(v)
			labelModel.LabelValue = v
		case "node":
			labelModel.ServiceID = serviceID
			labelModel.LabelKey = v
			labelModel.LabelValue = core_model.LabelKeyNodeSelector
		}
		if err := db.GetManager().TenantServiceLabelDao().AddModel(&labelModel); err != nil {
			return err
		}
	}
	return nil
}

//DeleteLabel delete label
func (s *ServiceAction) DeleteLabel(kind, serviceID string, amp []string) error {
	switch kind {
	case "node":
		return db.GetManager().TenantServiceLabelDao().DELTenantServiceLabelsByLabelvaluesAndServiceID(serviceID, amp)
	}
	return nil
}

//UpdateServiceLabel UpdateLabel
func (s *ServiceAction) UpdateServiceLabel(serviceID, value string) error {
	sls, err := db.GetManager().TenantServiceLabelDao().GetTenantServiceLabel(serviceID)
	if err != nil {
		return err
	}
	if len(sls) > 0 {
		for _, sl := range sls {
			sl.ServiceID = serviceID
			sl.LabelKey = core_model.LabelKeyServiceType
			value = chekeServiceLabel(value)
			sl.LabelValue = value
			return db.GetManager().TenantServiceLabelDao().UpdateModel(sl)
		}
	}
	return fmt.Errorf("Get tenant service label error")
}

//StartStopService start service
func (s *ServiceAction) StartStopService(sss *api_model.StartStopStruct) error {
	services, err := db.GetManager().TenantServiceDao().GetServiceByID(sss.ServiceID)
	if err != nil {
		logrus.Errorf("get service by id error, %v", err)
		return err
	}
	TaskBody := model.StopTaskBody{
		TenantID:      sss.TenantID,
		ServiceID:     sss.ServiceID,
		DeployVersion: services.DeployVersion,
		EventID:       sss.EventID,
	}
	ts := &api_db.TaskStruct{
		TaskType: sss.TaskType,
		TaskBody: TaskBody,
		User:     "define",
	}
	eq, errEq := api_db.BuildTask(ts)
	if errEq != nil {
		logrus.Errorf("build equeue stop request error, %v", errEq)
		return errEq
	}
	_, err = s.MQClient.Enqueue(context.Background(), eq)
	if err != nil {
		logrus.Errorf("equque mq error, %v", err)
		return err
	}
	logrus.Debugf("equeue mq stop task success")
	return nil
}

//ServiceVertical vertical service
func (s *ServiceAction) ServiceVertical(vs *model.VerticalScalingTaskBody) error {
	ts := &api_db.TaskStruct{
		TaskType: "vertical_scaling",
		TaskBody: vs,
		User:     "define",
	}
	eq, errEq := api_db.BuildTask(ts)
	if errEq != nil {
		logrus.Errorf("build equeue vertical request error, %v", errEq)
		return errEq
	}
	_, err := s.MQClient.Enqueue(context.Background(), eq)
	if err != nil {
		logrus.Errorf("equque mq error, %v", err)
		return err
	}
	logrus.Debugf("equeue mq vertical task success")
	return nil
}

//ServiceHorizontal Service Horizontal
func (s *ServiceAction) ServiceHorizontal(hs *model.HorizontalScalingTaskBody) error {
	ts := &api_db.TaskStruct{
		TaskType: "horizontal_scaling",
		TaskBody: hs,
		User:     "define",
	}
	eq, errEq := api_db.BuildTask(ts)
	if errEq != nil {
		logrus.Errorf("build equeue horizontal request error, %v", errEq)
		return errEq
	}
	_, err := s.MQClient.Enqueue(context.Background(), eq)
	if err != nil {
		logrus.Errorf("equque mq error, %v", err)
		return err
	}
	logrus.Debugf("equeue mq horizontal task success")
	return nil
}

//ServiceUpgrade service upgrade
func (s *ServiceAction) ServiceUpgrade(ru *model.RollingUpgradeTaskBody) error {
	services, err := db.GetManager().TenantServiceDao().GetServiceByID(ru.ServiceID)
	if err != nil {
		logrus.Errorf("get service by id error, %v, %v", services, err)
		return err
	}
	ru.CurrentDeployVersion = services.DeployVersion
	ts := &api_db.TaskStruct{
		TaskType: "rolling_upgrade",
		TaskBody: ru,
		User:     "define",
	}
	eq, errEq := api_db.BuildTask(ts)
	if errEq != nil {
		logrus.Errorf("build equeue upgrade request error, %v", errEq)
		return errEq
	}
	if _, err := s.MQClient.Enqueue(context.Background(), eq); err != nil {
		logrus.Errorf("equque mq error, %v", err)
		return err
	}
	return nil
}

//ServiceCreate create service
func (s *ServiceAction) ServiceCreate(sc *api_model.ServiceStruct) error {

	jsonSC, err := ffjson.Marshal(sc)
	if err != nil {
		logrus.Errorf("trans service struct to json failed. %v", err)
		return err
	}

	var ts dbmodel.TenantServices
	if err := ffjson.Unmarshal(jsonSC, &ts); err != nil {
		logrus.Errorf("trans json to tenant service error, %v", err)
		return err
	}

	ts.UpdateTime = time.Now()
	ports := sc.PortsInfo
	envs := sc.EnvsInfo
	volumns := sc.VolumesInfo
	dependIds := sc.DependIDs

	tx := db.GetManager().Begin()

	//服务信息表
	if err := db.GetManager().TenantServiceDaoTransactions(tx).AddModel(&ts); err != nil {
		logrus.Errorf("add service error, %v", err)
		tx.Rollback()
		return err
	}
	//TODO:
	//env
	if len(envs) > 0 {
		for _, env := range envs {
			env.ServiceID = ts.ServiceID
			env.TenantID = ts.TenantID
			if err := db.GetManager().TenantServiceEnvVarDaoTransactions(tx).AddModel(&env); err != nil {
				logrus.Errorf("add env %v error, %v", env.AttrName, err)
				tx.Rollback()
				return err
			}
		}
	}
	//port
	if len(ports) > 0 {
		for _, port := range ports {
			port.ServiceID = ts.ServiceID
			port.TenantID = ts.TenantID
			if err := db.GetManager().TenantServicesPortDaoTransactions(tx).AddModel(&port); err != nil {
				logrus.Errorf("add port %v error, %v", port.ContainerPort, err)
				tx.Rollback()
				return err
			}
		}
	}
	//volumns
	if len(volumns) > 0 {
		localPath := os.Getenv("LOCAL_DATA_PATH")
		sharePath := os.Getenv("SHARE_DATA_PATH")
		if localPath == "" {
			localPath = "/grlocaldata"
		}
		if sharePath == "" {
			sharePath = "/grdata"
		}

		for _, volumn := range volumns {
			volumn.ServiceID = ts.ServiceID
			if volumn.VolumeType == "" {
				volumn.VolumeType = dbmodel.ShareFileVolumeType.String()
			}
			if volumn.HostPath == "" {
				//step 1 设置主机目录
				switch volumn.VolumeType {
				//共享文件存储
				case dbmodel.ShareFileVolumeType.String():
					volumn.HostPath = fmt.Sprintf("%s/tenant/%s/service/%s%s", sharePath, sc.TenantID, volumn.ServiceID, volumn.VolumePath)
				//本地文件存储
				case dbmodel.LocalVolumeType.String():
					serviceType, err := db.GetManager().TenantServiceLabelDao().GetTenantServiceTypeLabel(volumn.ServiceID)
					if err != nil {
						return util.CreateAPIHandleErrorFromDBError("service type", err)
					}
					if serviceType.LabelValue != core_util.StatefulServiceType {
						return util.CreateAPIHandleError(400, fmt.Errorf("应用类型不为有状态应用.不支持本地存储"))
					}
					volumn.HostPath = fmt.Sprintf("%s/tenant/%s/service/%s%s", localPath, sc.TenantID, volumn.ServiceID, volumn.VolumePath)
				}
			}
			if volumn.VolumeName == "" {
				volumn.VolumeName = uuid.NewV4().String()
			}
			if err := db.GetManager().TenantServiceVolumeDaoTransactions(tx).AddModel(&volumn); err != nil {
				logrus.Errorf("add volumn %v error, %v", volumn.HostPath, err)
				tx.Rollback()
				return err
			}
		}
	}
	//depend_ids
	if len(dependIds) > 0 {
		for _, id := range dependIds {
			if err := db.GetManager().TenantServiceRelationDaoTransactions(tx).AddModel(&id); err != nil {
				logrus.Errorf("add depend_id %v error, %v", id.DependServiceID, err)
				tx.Rollback()
				return err
			}
		}
	}

	//status表
	if err := db.GetManager().TenantServiceStatusDaoTransactions(tx).AddModel(&dbmodel.TenantServiceStatus{
		ServiceID: ts.ServiceID,
		Status:    "undeploy",
	}); err != nil {
		logrus.Errorf("add status %v error, %v", ts.ServiceID, err)
		tx.Rollback()
		return err
	}
	//label表
	if err := db.GetManager().TenantServiceLabelDaoTransactions(tx).AddModel(&dbmodel.TenantServiceLable{
		ServiceID:  ts.ServiceID,
		LabelKey:   core_model.LabelKeyServiceType,
		LabelValue: sc.ServiceLabel,
	}); err != nil {
		logrus.Errorf("add label %v error, %v", ts.ServiceID, err)
		tx.Rollback()
		return err
	}
	tx.Commit()
	return nil
}

//ServiceUpdate update service
func (s *ServiceAction) ServiceUpdate(sc map[string]interface{}) error {
	ts, err := db.GetManager().TenantServiceDao().GetServiceByID(sc["service_id"].(string))
	if err != nil {
		return err
	}
	//TODO: 更新单个项方法不给力
	if sc["image_name"] != nil {
		ts.ImageName = sc["image_name"].(string)
	}
	if sc["container_memory"] != nil {
		ts.ContainerMemory = sc["container_memory"].(int)
	}
	if sc["container_cmd"] != nil {
		ts.ContainerCMD = sc["container_cmd"].(string)
	}
	//服务信息表
	if err := db.GetManager().TenantServiceDao().UpdateModel(ts); err != nil {
		logrus.Errorf("update service error, %v", err)
		return err
	}
	return nil
}

//LanguageSet language set
func (s *ServiceAction) LanguageSet(langS *api_model.LanguageSet) error {
	logrus.Debugf("service id is %s, language is %s", langS.ServiceID, langS.Language)
	services, err := db.GetManager().TenantServiceDao().GetServiceByID(langS.ServiceID)
	if err != nil {
		logrus.Errorf("get service by id error, %v, %v", services, err)
		return err
	}
	if langS.Language == "java" {
		services.ContainerMemory = 512
		if err := db.GetManager().TenantServiceDao().UpdateModel(services); err != nil {
			logrus.Errorf("update tenant service error %v", err)
			return err
		}
	}
	return nil
}

//GetService get service(s)
func (s *ServiceAction) GetService(tenantID string) ([]*dbmodel.TenantServices, error) {
	services, err := db.GetManager().TenantServiceDao().GetServicesAllInfoByTenantID(tenantID)
	if err != nil {
		logrus.Errorf("get service by id error, %v, %v", services, err)
		return nil, err
	}
	return services, nil
}

//CodeCheck code check
func (s *ServiceAction) CodeCheck(c *api_model.CheckCodeStruct) error {
	bodyJ, err := ffjson.Marshal(&c.Body)
	if err != nil {
		return err
	}
	bs := &api_db.BuildTaskStruct{
		TaskType: "code_check",
		TaskBody: bodyJ,
		User:     "define",
	}
	eq, errEq := api_db.BuildTaskBuild(bs)
	if errEq != nil {
		logrus.Errorf("build equeue code check error, %v", errEq)
		return errEq
	}
	_, err = s.MQClient.Enqueue(context.Background(), eq)
	if err != nil {
		logrus.Errorf("equque mq error, %v", err)
		return err
	}
	return nil
}

//ShareCloud share cloud
func (s *ServiceAction) ShareCloud(c *api_model.CloudShareStruct) error {
	var bs api_db.BuildTaskStruct
	switch c.Body.Kind {
	case "app_slug":
		bodyJ, err := ffjson.Marshal(&c.Body.Slug)
		if err != nil {
			return err
		}
		bs.User = "define"
		bs.TaskBody = bodyJ
		bs.TaskType = "app_slug"
	case "app_image":
		if c.Body.Image.ServiceID != "" {
			service, err := db.GetManager().TenantServiceDao().GetServiceByID(c.Body.Image.ServiceID)
			if err != nil {
				return err
			}
			c.Body.Image.Image = service.ImageName
		}
		bodyJ, err := ffjson.Marshal(&c.Body.Image)
		if err != nil {
			return err
		}
		bs.User = "define"
		bs.TaskBody = bodyJ
		bs.TaskType = "app_image"
	default:
		return fmt.Errorf("need share kind")
	}
	eq, errEq := api_db.BuildTaskBuild(&bs)
	if errEq != nil {
		logrus.Errorf("build equeue share cloud error, %v", errEq)
		return errEq
	}
	_, err := s.MQClient.Enqueue(context.Background(), eq)
	if err != nil {
		logrus.Errorf("equque mq error, %v", err)
		return err
	}
	return nil
}

//ServiceDepend service depend
func (s *ServiceAction) ServiceDepend(action string, ds *api_model.DependService) error {
	switch action {
	case "add":
		tsr := &dbmodel.TenantServiceRelation{
			TenantID:          ds.TenantID,
			ServiceID:         ds.ServiceID,
			DependServiceID:   ds.DepServiceID,
			DependServiceType: ds.DepServiceType,
			DependOrder:       1,
		}
		if err := db.GetManager().TenantServiceRelationDao().AddModel(tsr); err != nil {
			logrus.Errorf("add depend error, %v", err)
			return err
		}
	case "delete":
		logrus.Debugf("serviceid is %v, depid is %v", ds.ServiceID, ds.DepServiceID)
		if err := db.GetManager().TenantServiceRelationDao().DeleteRelationByDepID(ds.ServiceID, ds.DepServiceID); err != nil {
			logrus.Errorf("delete depend error, %v", err)
			return err
		}
	}
	return nil
}

//EnvAttr env attr
func (s *ServiceAction) EnvAttr(action string, at *dbmodel.TenantServiceEnvVar) error {
	switch action {
	case "add":
		if err := db.GetManager().TenantServiceEnvVarDao().AddModel(at); err != nil {
			logrus.Errorf("add env %v error, %v", at.AttrName, err)
			return err
		}
	case "delete":
		if err := db.GetManager().TenantServiceEnvVarDao().DeleteModel(at.ServiceID, at.AttrName); err != nil {
			logrus.Errorf("delete env %v error, %v", at.AttrName, err)
			return err
		}
	}
	return nil
}

//PortVar port var
func (s *ServiceAction) PortVar(action, tenantID, serviceID string, vps *api_model.ServicePorts, oldPort int) error {
	crt, err := db.GetManager().TenantServicePluginRelationDao().CheckSomeModelPluginByServiceID(
		serviceID,
		dbmodel.UpNetPlugin,
	)
	if err != nil {
		return err
	}
	switch action {
	case "add":
		for _, vp := range vps.Port {
			var vpD dbmodel.TenantServicesPort
			vpD.ServiceID = serviceID
			vpD.TenantID = tenantID
			//默认不打开
			vpD.IsInnerService = false
			vpD.IsOuterService = false
			vpD.ContainerPort = vp.ContainerPort
			vpD.MappingPort = vp.MappingPort
			vpD.Protocol = vp.Protocol
			vpD.PortAlias = vp.PortAlias
			if err := db.GetManager().TenantServicesPortDao().AddModel(&vpD); err != nil {
				logrus.Errorf("add port var error, %v", err)
				return err
			}
		}
	case "delete":
		tx := db.GetManager().Begin()
		for _, vp := range vps.Port {
			if err := db.GetManager().TenantServicesPortDaoTransactions(tx).DeleteModel(serviceID, vp.ContainerPort); err != nil {
				logrus.Errorf("delete port var error, %v", err)
				tx.Rollback()
				return err
			}
			//TODO:删除k8s Service
			service, err := db.GetManager().K8sServiceDao().GetK8sService(serviceID, vp.ContainerPort, true)
			if err != nil && err.Error() != gorm.ErrRecordNotFound.Error() {
				logrus.Error("get deploy k8s service info error.")
				tx.Rollback()
				return err
			}
			if service != nil {
				err := s.KubeClient.Core().Services(tenantID).Delete(service.K8sServiceID, &metav1.DeleteOptions{})
				if err != nil {
					logrus.Error("delete deploy k8s service info from kube-api error.")
					tx.Rollback()
					return err
				}
				err = db.GetManager().K8sServiceDaoTransactions(tx).DeleteK8sServiceByName(service.K8sServiceID)
				if err != nil {
					logrus.Error("delete deploy k8s service info from db error.")
					tx.Rollback()
					return err
				}
				if crt {
					if err := db.GetManager().TenantServicesStreamPluginPortDaoTransactions(tx).DeletePluginMappingPortByContainerPort(
						serviceID,
						dbmodel.UpNetPlugin,
						vp.ContainerPort,
					); err != nil {
						logrus.Errorf("delete plugin stream mapping port error: (%s)", err)
						tx.Rollback()
						return err
					}
				}
			}
		}
		if err := tx.Commit().Error; err != nil {
			tx.Rollback()
			logrus.Debugf("commit delete port error, %v", err)
			return err
		}
	case "update":
		tx := db.GetManager().Begin()
		for _, vp := range vps.Port {
			//port更新单个请求
			if oldPort == 0 {
				oldPort = vp.ContainerPort
			}
			vpD, err := db.GetManager().TenantServicesPortDao().GetPort(serviceID, oldPort)
			if err != nil {
				return err
			}
			vpD.ServiceID = serviceID
			vpD.TenantID = tenantID
			vpD.IsInnerService = vp.IsInnerService
			vpD.IsOuterService = vp.IsOuterService
			vpD.ContainerPort = vp.ContainerPort
			vpD.MappingPort = vp.MappingPort
			vpD.Protocol = vp.Protocol
			vpD.PortAlias = vp.PortAlias
			if err := db.GetManager().TenantServicesPortDaoTransactions(tx).UpdateModel(vpD); err != nil {
				logrus.Errorf("update port var error, %v", err)
				tx.Rollback()
				return err
			}
			if crt {
				pluginPort, err := db.GetManager().TenantServicesStreamPluginPortDao().GetPluginMappingPortByServiceIDAndContainerPort(
					serviceID,
					dbmodel.UpNetPlugin,
					oldPort,
				)
				if err != nil {
					logrus.Errorf("get plugin mapping port error:(%s)", err)
					tx.Rollback()
					return err
				}
				pluginPort.ContainerPort = vp.ContainerPort
				if err := db.GetManager().TenantServicesStreamPluginPortDaoTransactions(tx).UpdateModel(pluginPort); err != nil {
					logrus.Errorf("update plugin mapping port error:(%s)", err)
					tx.Rollback()
					return err
				}

			}
		}
		if err := tx.Commit().Error; err != nil {
			tx.Rollback()
			logrus.Debugf("commit update port error, %v", err)
			return err
		}
	}
	return nil
}

//PortOuter 端口对外服务操作
func (s *ServiceAction) PortOuter(tenantName, serviceID, operation string, port int) (*dbmodel.TenantServiceLBMappingPort, string, error) {
	p, err := db.GetManager().TenantServicesPortDao().GetPort(serviceID, port)
	if err != nil {
		return nil, "", fmt.Errorf("find service port error:%s", err.Error())
	}
	service, err := db.GetManager().TenantServiceDao().GetServiceByID(serviceID)
	if err != nil {
		return nil, "", fmt.Errorf("find service error:%s", err.Error())
	}
	hasUpStream, err := db.GetManager().TenantServicePluginRelationDao().CheckSomeModelPluginByServiceID(
		serviceID,
		dbmodel.UpNetPlugin,
	)
	if err != nil {
		return nil, "", fmt.Errorf("get plugin relations error: %s", err.Error())
	}
	var k8sService *v1.Service
	//if stream 创建vs端口
	vsPort := &dbmodel.TenantServiceLBMappingPort{}
	switch operation {
	case "close":
		if p.IsOuterService { //如果端口已经开了对外
			p.IsOuterService = false
			tx := db.GetManager().Begin()
			if err = db.GetManager().TenantServicesPortDaoTransactions(tx).UpdateModel(p); err != nil {
				tx.Callback()
				return nil, "", err
			}
			service, err := db.GetManager().K8sServiceDao().GetK8sService(serviceID, port, true)
			if err != nil && err != gorm.ErrRecordNotFound {
				logrus.Error("get deploy k8s service info error.")
			}
			if service != nil {
				err := s.KubeClient.Core().Services(p.TenantID).Delete(service.K8sServiceID, &metav1.DeleteOptions{})
				if err != nil {
					tx.Callback()
					return nil, "", fmt.Errorf("delete deploy k8s service info from kube-api error.%s", err.Error())
				}
				err = db.GetManager().K8sServiceDaoTransactions(tx).DeleteK8sServiceByName(service.K8sServiceID)
				if err != nil {
					tx.Callback()
					return nil, "", fmt.Errorf("delete deploy k8s service info from db error")
				}
			}
			if hasUpStream {
				pluginPort, err := db.GetManager().TenantServicesStreamPluginPortDao().GetPluginMappingPortByServiceIDAndContainerPort(
					serviceID,
					dbmodel.UpNetPlugin,
					port,
				)
				if err != nil {
					if err.Error() == gorm.ErrRecordNotFound.Error() {
						logrus.Debugf("outer, plugin port (%d) is not exist, do not need delete", port)
						goto OUTERCLOSEPASS
					}
					tx.Callback()
					return nil, "", fmt.Errorf("outer, get plugin mapping port error:(%s)", err)
				}
				if p.IsInnerService {
					//发现内网未关闭则不删除该映射
					logrus.Debugf("outer, close outer, but plugin inner port (%d) is exist, do not need delete", port)
					goto OUTERCLOSEPASS
				}
				if err := db.GetManager().TenantServicesStreamPluginPortDaoTransactions(tx).DeletePluginMappingPortByContainerPort(
					serviceID,
					dbmodel.UpNetPlugin,
					port,
				); err != nil {
					tx.Callback()
					return nil, "", fmt.Errorf("outer, delete plugin mapping port %d error:(%s)", port, err)
				}
				logrus.Debugf(fmt.Sprintf("outer, delete plugin port %d->%d", port, pluginPort.PluginPort))
			OUTERCLOSEPASS:
			}
			if err := tx.Commit().Error; err != nil {
				tx.Rollback()
				//删除已创建的SERVICE
				if k8sService != nil {
					s.KubeClient.Core().Services(k8sService.Namespace).Delete(k8sService.Name, &metav1.DeleteOptions{})
				}
				return nil, "", err
			}
		} else {
			return nil, "", nil
		}

	case "open":
		if p.IsOuterService {
			if p.Protocol != "http" && p.Protocol != "https" {
				vsPort, err = s.createVSPort(serviceID, p.ContainerPort)
				if vsPort == nil {
					return nil, "", fmt.Errorf("port already open but can not get lb mapping port,%s", err.Error())
				}
				return vsPort, p.Protocol, nil
			}
		}
		p.IsOuterService = true
		tx := db.GetManager().Begin()
		if err = db.GetManager().TenantServicesPortDaoTransactions(tx).UpdateModel(p); err != nil {
			tx.Callback()
			return nil, "", err
		}
		if p.Protocol != "http" && p.Protocol != "https" {
			vsPort, err = s.createVSPort(serviceID, p.ContainerPort)
			if vsPort == nil {
				tx.Callback()
				return nil, "", fmt.Errorf("create or get vs map port for service error,%s", err.Error())
			}
		}
		deploy, _ := db.GetManager().K8sDeployReplicationDao().GetK8sCurrentDeployReplicationByService(serviceID)
		if deploy != nil {
			k8sService, err = s.createOuterK8sService(tenantName, vsPort, service, p, deploy)
			if err != nil && !strings.HasSuffix(err.Error(), "is exist") {
				tx.Callback()
				return nil, "", fmt.Errorf("create k8s service error,%s", err.Error())
			}
		}
		if hasUpStream {
			pluginPort, err := db.GetManager().TenantServicesStreamPluginPortDao().GetPluginMappingPortByServiceIDAndContainerPort(
				serviceID,
				dbmodel.UpNetPlugin,
				port,
			)
			var pPort int
			if err != nil {
				if err.Error() == gorm.ErrRecordNotFound.Error() {
					ppPort, err := db.GetManager().TenantServicesStreamPluginPortDaoTransactions(tx).SetPluginMappingPort(
						p.TenantID,
						serviceID,
						dbmodel.UpNetPlugin,
						port,
					)
					if err != nil {
						tx.Callback()
						logrus.Errorf("outer, set plugin mapping port error:(%s)", err)
						return nil, "", fmt.Errorf("outer, set plugin mapping port error:(%s)", err)
					}
					pPort = ppPort
					goto OUTEROPENPASS
				}
				tx.Callback()
				return nil, "", fmt.Errorf("outer, in setting plugin mapping port, get plugin mapping port error:(%s)", err)
			}
			logrus.Debugf("outer, plugin mapping port is already exist, %d->%d", pluginPort.ContainerPort, pluginPort.PluginPort)
		OUTEROPENPASS:
			logrus.Debugf("outer, set plugin mapping port %d->%d", port, pPort)
		}
		if err := tx.Commit().Error; err != nil {
			tx.Rollback()
			//删除已创建的SERVICE
			if k8sService != nil {
				s.KubeClient.Core().Services(k8sService.Namespace).Delete(k8sService.Name, &metav1.DeleteOptions{})
			}
			return nil, "", err
		}
	}
	return vsPort, p.Protocol, nil
}
func (s *ServiceAction) createVSPort(serviceID string, containerPort int) (*dbmodel.TenantServiceLBMappingPort, error) {
	vsPort, err := db.GetManager().TenantServiceLBMappingPortDao().CreateTenantServiceLBMappingPort(serviceID, containerPort)
	if err != nil {
		return nil, fmt.Errorf("create vs map port for service error,%s", err.Error())
	}
	return vsPort, nil
}
func (s *ServiceAction) createOuterK8sService(tenantName string, mapPort *dbmodel.TenantServiceLBMappingPort, tenantservice *dbmodel.TenantServices, port *dbmodel.TenantServicesPort, deploy *dbmodel.K8sDeployReplication) (*v1.Service, error) {
	var service v1.Service
	service.Name = fmt.Sprintf("service-%d-%dout", port.ID, port.ContainerPort)
	service.Labels = map[string]string{
		"service_type":     "outer",
		"name":             tenantservice.ServiceAlias + "ServiceOUT",
		"tenant_name":      tenantName,
		"services_version": tenantservice.ServiceVersion,
		"domain":           tenantservice.Autodomain(tenantName, port.ContainerPort),
		"protocol":         port.Protocol,
		"ca":               "",
		"key":              "",
		"event_id":         tenantservice.EventID,
	}
	if port.Protocol == "stream" && mapPort != nil { //stream 协议获取映射端口
		service.Labels["lbmap_port"] = fmt.Sprintf("%d", mapPort.Port)
	}
	var servicePort v1.ServicePort
	if port.Protocol == "udp" {
		servicePort.Protocol = "UDP"
	} else {
		servicePort.Protocol = "TCP"
	}
	servicePort.TargetPort = intstr.FromInt(port.ContainerPort)
	servicePort.Port = int32(port.ContainerPort)
	var portType v1.ServiceType
	if os.Getenv("CUR_NET") == "midonet" {
		portType = v1.ServiceTypeNodePort
	} else {
		portType = v1.ServiceTypeClusterIP
	}
	spec := v1.ServiceSpec{
		Ports:    []v1.ServicePort{servicePort},
		Selector: map[string]string{"name": tenantservice.ServiceAlias},
		Type:     portType,
	}
	service.Spec = spec
	k8sService, err := s.KubeClient.Core().Services(tenantservice.TenantID).Create(&service)
	if err != nil && !strings.HasSuffix(err.Error(), "already exists") {
		return nil, err
	}
	if err := db.GetManager().K8sServiceDao().AddModel(&dbmodel.K8sService{
		TenantID:        tenantservice.TenantID,
		ServiceID:       tenantservice.ServiceID,
		K8sServiceID:    service.Name,
		ContainerPort:   port.ContainerPort,
		ReplicationID:   deploy.ReplicationID,
		ReplicationType: deploy.ReplicationType,
		IsOut:           true,
	}); err != nil {
		if !strings.HasSuffix(err.Error(), "is exist") {
			s.KubeClient.Core().Services(tenantservice.TenantID).Delete(k8sService.Name, &metav1.DeleteOptions{})
			return nil, err
		}
	}
	return k8sService, nil
}

func (s *ServiceAction) createInnerService(tenantservice *dbmodel.TenantServices, port *dbmodel.TenantServicesPort, deploy *dbmodel.K8sDeployReplication) (*v1.Service, error) {
	var service v1.Service
	service.Name = fmt.Sprintf("service-%d-%d", port.ID, port.ContainerPort)
	service.Labels = map[string]string{
		"service_type": "inner",
		"name":         tenantservice.ServiceAlias + "Service",
	}
	var servicePort v1.ServicePort
	if port.Protocol == "udp" {
		servicePort.Protocol = "UDP"
	} else {
		servicePort.Protocol = "TCP"
	}
	servicePort.TargetPort = intstr.FromInt(port.ContainerPort)
	servicePort.Port = int32(port.MappingPort)
	if servicePort.Port == 0 {
		servicePort.Port = int32(port.ContainerPort)
	}
	spec := v1.ServiceSpec{
		Ports:    []v1.ServicePort{servicePort},
		Selector: map[string]string{"name": tenantservice.ServiceAlias},
	}
	service.Spec = spec
	k8sService, err := s.KubeClient.Core().Services(tenantservice.TenantID).Create(&service)
	if err != nil && !strings.HasSuffix(err.Error(), "already exists") {
		return nil, err
	}
	if err := db.GetManager().K8sServiceDao().AddModel(&dbmodel.K8sService{
		TenantID:        tenantservice.TenantID,
		ServiceID:       tenantservice.ServiceID,
		K8sServiceID:    service.Name,
		ContainerPort:   port.ContainerPort,
		ReplicationID:   deploy.ReplicationID,
		ReplicationType: deploy.ReplicationType,
		IsOut:           false,
	}); err != nil {
		if !strings.HasSuffix(err.Error(), "is exist") {
			s.KubeClient.Core().Services(tenantservice.TenantID).Delete(k8sService.Name, &metav1.DeleteOptions{})
			return nil, err
		}
	}
	return k8sService, nil
}

//PortInner 端口对内服务操作
func (s *ServiceAction) PortInner(tenantName, serviceID, operation string, port int) error {
	p, err := db.GetManager().TenantServicesPortDao().GetPort(serviceID, port)
	if err != nil {
		return err
	}
	service, err := db.GetManager().TenantServiceDao().GetServiceByID(serviceID)
	if err != nil {
		return fmt.Errorf("get service error:%s", err.Error())
	}
	hasUpStream, err := db.GetManager().TenantServicePluginRelationDao().CheckSomeModelPluginByServiceID(
		serviceID,
		dbmodel.UpNetPlugin,
	)
	if err != nil {
		return fmt.Errorf("get plugin relations error: %s", err.Error())
	}
	var k8sService *v1.Service
	tx := db.GetManager().Begin()
	switch operation {
	case "close":
		if p.IsInnerService { //如果端口已经开了对内
			p.IsInnerService = false
			if err = db.GetManager().TenantServicesPortDaoTransactions(tx).UpdateModel(p); err != nil {
				tx.Callback()
				return fmt.Errorf("update service port error: %s", err.Error())
			}
			service, err := db.GetManager().K8sServiceDao().GetK8sService(serviceID, port, false)
			if err != nil && err != gorm.ErrRecordNotFound {
				logrus.Error("get deploy k8s service info error.", err.Error())
			}
			if service != nil {
				err := s.KubeClient.Core().Services(p.TenantID).Delete(service.K8sServiceID, &metav1.DeleteOptions{})
				if err != nil && !strings.HasSuffix(err.Error(), "not found") {
					tx.Callback()
					return fmt.Errorf("delete deploy k8s service info from kube-api error")
				}
				err = db.GetManager().K8sServiceDao().DeleteK8sServiceByName(service.K8sServiceID)
				if err != nil {
					tx.Callback()
					return fmt.Errorf("delete deploy k8s service info from db error")
				}
			}
			if hasUpStream {
				pluginPort, err := db.GetManager().TenantServicesStreamPluginPortDao().GetPluginMappingPortByServiceIDAndContainerPort(
					serviceID,
					dbmodel.UpNetPlugin,
					port,
				)
				if err != nil {
					if err.Error() == gorm.ErrRecordNotFound.Error() {
						logrus.Debugf("inner, plugin port (%d) is not exist, do not need delete", port)
						goto INNERCLOSEPASS
					}
					tx.Callback()
					return fmt.Errorf("inner, get plugin mapping port error:(%s)", err)
				}
				if p.IsOuterService {
					logrus.Debugf("inner, close inner, but plugin outerport (%d) is exist, do not need delete", port)
					goto INNERCLOSEPASS
				}
				if err := db.GetManager().TenantServicesStreamPluginPortDaoTransactions(tx).DeletePluginMappingPortByContainerPort(
					serviceID,
					dbmodel.UpNetPlugin,
					port,
				); err != nil {
					tx.Callback()
					return fmt.Errorf("inner, delete plugin mapping port %d error:(%s)", port, err)
				}
				logrus.Debugf(fmt.Sprintf("inner, delete plugin port %d->%d", port, pluginPort.PluginPort))
			INNERCLOSEPASS:
			}
		} else {
			tx.Callback()
			return fmt.Errorf("already close")
		}
	case "open":
		if p.IsInnerService {
			tx.Callback()
			return fmt.Errorf("already open")
		}
		p.IsInnerService = true
		if err = db.GetManager().TenantServicesPortDaoTransactions(tx).UpdateModel(p); err != nil {
			tx.Callback()
			return err
		}
		deploy, _ := db.GetManager().K8sDeployReplicationDao().GetK8sCurrentDeployReplicationByService(serviceID)
		if deploy != nil {
			k8sService, err = s.createInnerService(service, p, deploy)
			if err != nil {
				tx.Callback()
				return fmt.Errorf("create k8s service error,%s", err.Error())
			}

		}
		if hasUpStream {
			pluginPort, err := db.GetManager().TenantServicesStreamPluginPortDao().GetPluginMappingPortByServiceIDAndContainerPort(
				serviceID,
				dbmodel.UpNetPlugin,
				port,
			)
			var pPort int
			if err != nil {
				if err.Error() == gorm.ErrRecordNotFound.Error() {
					ppPort, err := db.GetManager().TenantServicesStreamPluginPortDaoTransactions(tx).SetPluginMappingPort(
						p.TenantID,
						serviceID,
						dbmodel.UpNetPlugin,
						port,
					)
					if err != nil {
						tx.Callback()
						logrus.Errorf("inner, set plugin mapping port error:(%s)", err)
						return fmt.Errorf("inner, set plugin mapping port error:(%s)", err)
					}
					pPort = ppPort
					goto INNEROPENPASS
				}
				tx.Callback()
				return fmt.Errorf("inner, in setting plugin mapping port, get plugin mapping port error:(%s)", err)
			}
			logrus.Debugf("inner, plugin mapping port is already exist, %d->%d", pluginPort.ContainerPort, pluginPort.PluginPort)
		INNEROPENPASS:
			logrus.Debugf("inner, set plugin mapping port %d->%d", port, pPort)
		}
	}
	if err := tx.Commit().Error; err != nil {
		tx.Rollback()
		//删除已创建的SERVICE
		if k8sService != nil {
			s.KubeClient.Core().Services(k8sService.Namespace).Delete(k8sService.Name, &metav1.DeleteOptions{})
		}
		return err
	}
	return nil
}

//VolumnVar var volumn
func (s *ServiceAction) VolumnVar(tsv *dbmodel.TenantServiceVolume, tenantID, action string) *util.APIHandleError {
	localPath := os.Getenv("LOCAL_DATA_PATH")
	sharePath := os.Getenv("SHARE_DATA_PATH")
	if localPath == "" {
		localPath = "/grlocaldata"
	}
	if sharePath == "" {
		sharePath = "/grdata"
	}
	switch action {
	case "add":
		if tsv.HostPath == "" {
			//step 1 设置主机目录
			switch tsv.VolumeType {
			//共享文件存储
			case dbmodel.ShareFileVolumeType.String():
				tsv.HostPath = fmt.Sprintf("%s/tenant/%s/service/%s%s", sharePath, tenantID, tsv.ServiceID, tsv.VolumePath)
			//本地文件存储
			case dbmodel.LocalVolumeType.String():
				serviceType, err := db.GetManager().TenantServiceLabelDao().GetTenantServiceTypeLabel(tsv.ServiceID)
				if err != nil {
					return util.CreateAPIHandleErrorFromDBError("service type", err)
				}
				if serviceType == nil || serviceType.LabelValue != core_util.StatefulServiceType {
					return util.CreateAPIHandleError(400, fmt.Errorf("应用类型不为有状态应用.不支持本地存储"))
				}
				tsv.HostPath = fmt.Sprintf("%s/tenant/%s/service/%s%s", localPath, tenantID, tsv.ServiceID, tsv.VolumePath)
			}
		}
		if tsv.VolumeName == "" {
			tsv.VolumeName = uuid.NewV4().String()
		}
		if err := db.GetManager().TenantServiceVolumeDao().AddModel(tsv); err != nil {
			return util.CreateAPIHandleErrorFromDBError("add volume", err)
		}
	case "delete":
		if tsv.VolumeName != "" {
			if err := db.GetManager().TenantServiceVolumeDao().DeleteModel(tsv.ServiceID, tsv.VolumeName); err != nil {
				return util.CreateAPIHandleErrorFromDBError("delete volume", err)
			}
		} else {
			if err := db.GetManager().TenantServiceVolumeDao().DeleteByServiceIDAndVolumePath(tsv.ServiceID, tsv.VolumePath); err != nil {
				return util.CreateAPIHandleErrorFromDBError("delete volume", err)
			}
		}
	}
	return nil
}

//GetVolumes 获取应用全部存储
func (s *ServiceAction) GetVolumes(serviceID string) ([]*dbmodel.TenantServiceVolume, *util.APIHandleError) {
	dbManager := db.GetManager()
	service, err := dbManager.TenantServiceDao().GetServiceByID(serviceID)
	if err != nil {
		return nil, util.CreateAPIHandleErrorFromDBError("get service", err)
	}
	vs, err := dbManager.TenantServiceVolumeDao().GetTenantServiceVolumesByServiceID(serviceID)
	if err != nil && err.Error() != gorm.ErrRecordNotFound.Error() {
		return nil, util.CreateAPIHandleErrorFromDBError("get volumes", err)
	}
	if service.VolumePath != "" && service.VolumeMountPath != "" {
		vs = append(vs, &dbmodel.TenantServiceVolume{
			ServiceID:  serviceID,
			VolumeType: service.VolumeType,
			//VolumeName: service.VolumePath,
			VolumePath: service.VolumeMountPath,
			HostPath:   service.HostPath,
		})
	}
	return vs, nil
}

//VolumeDependency VolumeDependency
func (s *ServiceAction) VolumeDependency(tsr *dbmodel.TenantServiceMountRelation, action string) *util.APIHandleError {
	switch action {
	case "add":
		if tsr.VolumeName != "" {
			vm, err := db.GetManager().TenantServiceVolumeDao().GetVolumeByServiceIDAndName(tsr.DependServiceID, tsr.VolumeName)
			if err != nil {
				return util.CreateAPIHandleErrorFromDBError("get volume", err)
			}
			tsr.HostPath = vm.HostPath
			if err := db.GetManager().TenantServiceMountRelationDao().AddModel(tsr); err != nil {
				return util.CreateAPIHandleErrorFromDBError("add volume mount relation", err)
			}
		} else {
			if tsr.HostPath == "" {
				return util.CreateAPIHandleError(400, fmt.Errorf("host path can not be empty when create volume dependency in api v2"))
			}
			if err := db.GetManager().TenantServiceMountRelationDao().AddModel(tsr); err != nil {
				return util.CreateAPIHandleErrorFromDBError("add volume mount relation", err)
			}
		}
	case "delete":
		if tsr.VolumeName != "" {
			if err := db.GetManager().TenantServiceMountRelationDao().DElTenantServiceMountRelationByServiceAndName(tsr.ServiceID, tsr.VolumeName); err != nil {
				return util.CreateAPIHandleErrorFromDBError("delete mount relation", err)
			}
		} else {
			if err := db.GetManager().TenantServiceMountRelationDao().DElTenantServiceMountRelationByDepService(tsr.ServiceID, tsr.DependServiceID); err != nil {
				return util.CreateAPIHandleErrorFromDBError("delete mount relation", err)
			}
		}
	}
	return nil
}

//GetDepVolumes 获取依赖存储
func (s *ServiceAction) GetDepVolumes(serviceID string) ([]*dbmodel.TenantServiceMountRelation, *util.APIHandleError) {
	dbManager := db.GetManager()
	mounts, err := dbManager.TenantServiceMountRelationDao().GetTenantServiceMountRelationsByService(serviceID)
	if err != nil {
		return nil, util.CreateAPIHandleErrorFromDBError("get dep volume", err)
	}
	return mounts, nil
}

//ServiceProbe ServiceProbe
func (s *ServiceAction) ServiceProbe(tsp *dbmodel.ServiceProbe, action string) error {
	switch action {
	case "add":
		if err := db.GetManager().ServiceProbeDao().AddModel(tsp); err != nil {
			return err
		}
	case "update":
		if err := db.GetManager().ServiceProbeDao().UpdateModel(tsp); err != nil {
			return err
		}
	case "delete":
		if err := db.GetManager().ServiceProbeDao().DeleteModel(tsp.ServiceID, tsp.ProbeID); err != nil {
			return err
		}
	}
	return nil
}

//RollBack RollBack
func (s *ServiceAction) RollBack(rs *api_model.RollbackStruct) error {
	tx := db.GetManager().Begin()
	service, err := db.GetManager().TenantServiceDaoTransactions(tx).GetServiceByID(rs.ServiceID)
	if err != nil {
		return err
	}
	if service.DeployVersion == rs.DeployVersion {
		return fmt.Errorf("current version is %v, don't need rollback", rs.DeployVersion)
	}
	service.DeployVersion = rs.DeployVersion
	if err := db.GetManager().TenantServiceDaoTransactions(tx).UpdateModel(service); err != nil {
		return err
	}
	//发送重启消息到MQ
	startStopStruct := &api_model.StartStopStruct{
		TenantID:  rs.TenantID,
		ServiceID: rs.ServiceID,
		EventID:   rs.EventID,
		TaskType:  "restart",
	}
	if err := GetServiceManager().StartStopService(startStopStruct); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit().Error; err != nil {
		return err
	}
	return nil
}

//GetStatus GetStatus
func (s *ServiceAction) GetStatus(serviceID string) (*api_model.StatusList, error) {
	services, errS := db.GetManager().TenantServiceDao().GetServiceByID(serviceID)
	if errS != nil {
		return nil, errS
	}
	sl := &api_model.StatusList{
		TenantID:      services.TenantID,
		ServiceID:     serviceID,
		ServiceAlias:  services.ServiceAlias,
		DeployVersion: services.DeployVersion,
		Replicas:      services.Replicas,
		ContainerMem:  services.ContainerMemory,
		ContainerCPU:  services.ContainerCPU,
		CurStatus:     services.CurStatus,
		StatusCN:      TransStatus(services.CurStatus),
	}
	servicesStatus, err := db.GetManager().TenantServiceStatusDao().GetTenantServiceStatus(serviceID)
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	}
	if servicesStatus != nil {
		sl.CurStatus = servicesStatus.Status
		sl.StatusCN = TransStatus(sl.CurStatus)
	}
	return sl, nil
}

//GetServicesStatus  获取一组应用状态，若 serviceIDs为空,获取租户所有应用状态
func (s *ServiceAction) GetServicesStatus(tenantID string, serviceIDs []string) ([]*dbmodel.TenantServiceStatus, error) {
	if serviceIDs == nil || len(serviceIDs) == 0 {
		return db.GetManager().TenantServiceStatusDao().GetTenantStatus(tenantID)
	}
	statusList, err := db.GetManager().TenantServiceStatusDao().GetTenantServicesStatus(serviceIDs)
	if err != nil {
		return nil, err
	}
	/*
		for _, serviceID := range serviceIDs {
			if !CheckLabel(serviceID) {
				s, err := db.GetManager().TenantServiceDao().GetServiceByID(serviceID)
				if err != nil {
					continue
					//return nil, err
				}
				tss := &dbmodel.TenantServiceStatus{
					ServiceID: serviceID,
					Status:    s.CurStatus,
				}
				statusList = append(statusList, tss)
			}
		}
	*/
	return statusList, nil
}

//CreateTenant create tenant
func (s *ServiceAction) CreateTenant(t *dbmodel.Tenants) error {
	if ten, _ := db.GetManager().TenantDao().GetTenantIDByName(t.Name); ten != nil {
		return fmt.Errorf("tenant name %s is exist", t.Name)
	}
	tx := db.GetManager().Begin()
	if err := db.GetManager().TenantDaoTransactions(tx).AddModel(t); err != nil {
		if strings.HasSuffix(err.Error(), "is exist") {
			_, err := s.KubeClient.Core().Namespaces().Get(t.UUID, metav1.GetOptions{})
			if err == nil {
				tx.Commit()
				return nil
			}
		}
		tx.Callback()
		return err
	}
	ns := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:         t.UUID,
			GenerateName: t.Name,
		},
	}
	if _, err := s.KubeClient.Core().Namespaces().Create(ns); err != nil {
		if !strings.HasSuffix(err.Error(), "already exists") {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit().Error
}

//CreateTenandIDAndName create tenant_id and tenant_name
func (s *ServiceAction) CreateTenandIDAndName(eid string) (string, string, error) {
	id := fmt.Sprintf("%s", uuid.NewV4())
	uid := strings.Replace(id, "-", "", -1)
	name := strings.Split(id, "-")[0]
	logrus.Debugf("uuid is %v, name is %v", uid, name)
	return uid, name, nil
}

//GetPods get pods
func (s *ServiceAction) GetPods(serviceID string) ([]*dbmodel.K8sPod, error) {
	pods, err := db.GetManager().K8sPodDao().GetPodByService(serviceID)
	if err != nil {
		return nil, err
	}
	return pods, nil
}

//TransServieToDelete trans service info to delete table
func (s *ServiceAction) TransServieToDelete(serviceID string) error {
	status, err := db.GetManager().TenantServiceStatusDao().GetTenantServiceStatus(serviceID)
	if err != nil {
		return err
	}
	if status.Status != "closed" && status.Status != "undeploy" {
		return fmt.Errorf("unclosed")
	}
	service, err := db.GetManager().TenantServiceDao().GetServiceByID(serviceID)
	if err != nil {
		return err
	}
	tx := db.GetManager().Begin()
	//此处的原因，必须使用golang 1.8 以上版本编译
	delService := service.ChangeDelete()
	if err := db.GetManager().TenantServiceDeleteDaoTransactions(tx).AddModel(delService); err != nil {
		tx.Rollback()
		return err
	}
	if err := db.GetManager().TenantServiceDaoTransactions(tx).DeleteServiceByServiceID(serviceID); err != nil {
		tx.Rollback()
		return err
	}

	//删除domain
	//删除pause
	//删除tenant_system_pause
	//删除tenant_service_relation
	if err := db.GetManager().TenantServiceMountRelationDaoTransactions(tx).DELTenantServiceMountRelationByServiceID(serviceID); err != nil {
		if err.Error() != gorm.ErrRecordNotFound.Error() {
			tx.Rollback()
			return err
		}
	}
	//删除tenant_service_evn_var
	if err := db.GetManager().TenantServiceEnvVarDaoTransactions(tx).DELServiceEnvsByServiceID(serviceID); err != nil {
		if err.Error() != gorm.ErrRecordNotFound.Error() {
			tx.Rollback()
			return err
		}
	}
	//删除tenant_services_port
	if err := db.GetManager().TenantServicesPortDaoTransactions(tx).DELPortsByServiceID(serviceID); err != nil {
		if err.Error() != gorm.ErrRecordNotFound.Error() {
			tx.Rollback()
			return err
		}
	}
	//删除clear net bridge
	//删除tenant_service_mnt_relation
	if err := db.GetManager().TenantServiceRelationDaoTransactions(tx).DELRelationsByServiceID(serviceID); err != nil {
		if err.Error() != gorm.ErrRecordNotFound.Error() {
			tx.Rollback()
			return err
		}
	}
	//删除tenant_lb_mapping_port
	if err := db.GetManager().TenantServiceLBMappingPortDaoTransactions(tx).DELServiceLBMappingPortByServiceID(serviceID); err != nil {
		if err.Error() != gorm.ErrRecordNotFound.Error() {
			tx.Rollback()
			return err
		}
	}
	//删除tenant_service_volume
	if err := db.GetManager().TenantServiceVolumeDaoTransactions(tx).DeleteTenantServiceVolumesByServiceID(serviceID); err != nil {
		if err.Error() != gorm.ErrRecordNotFound.Error() {
			tx.Rollback()
			return err
		}
	}
	//删除tenant_service_pod
	if err := db.GetManager().K8sPodDaoTransactions(tx).DeleteK8sPod(serviceID); err != nil {
		if err.Error() != gorm.ErrRecordNotFound.Error() {
			tx.Rollback()
			return err
		}
	}
	//删除service_probe
	if err := db.GetManager().ServiceProbeDaoTransactions(tx).DELServiceProbesByServiceID(serviceID); err != nil {
		if err.Error() != gorm.ErrRecordNotFound.Error() {
			tx.Rollback()
			return err
		}
	}
	if err := tx.Commit().Error; err != nil {
		tx.Rollback()
		return err
	}
	return nil
}

//GetTenantServicePluginRelation GetTenantServicePluginRelation
func (s *ServiceAction) GetTenantServicePluginRelation(serviceID string) ([]*dbmodel.TenantServicePluginRelation, *util.APIHandleError) {
	gps, err := db.GetManager().TenantServicePluginRelationDao().GetALLRelationByServiceID(serviceID)
	if err != nil {
		return nil, util.CreateAPIHandleErrorFromDBError("get service relation by ID", err)
	}
	return gps, nil
}

//TenantServiceDeletePluginRelation 删除应用的plugin依赖
func (s *ServiceAction) TenantServiceDeletePluginRelation(serviceID, pluginID string) *util.APIHandleError {
	tx := db.GetManager().Begin()
	if err := db.GetManager().TenantServicePluginRelationDaoTransactions(tx).DeleteRelationByServiceIDAndPluginID(serviceID, pluginID); err != nil {
		tx.Rollback()
		return util.CreateAPIHandleErrorFromDBError("delete plugin relation", err)
	}
	if err := db.GetManager().TenantPluginVersionENVDaoTransactions(tx).DeleteEnvByPluginID(serviceID, pluginID); err != nil {
		tx.Rollback()
		return util.CreateAPIHandleErrorFromDBError("delete relation env", err)
	}
	if err := db.GetManager().TenantServicesStreamPluginPortDaoTransactions(tx).DeleteAllPluginMappingPortByServiceID(serviceID); err != nil {
		tx.Rollback()
		return util.CreateAPIHandleErrorFromDBError("delete upstream plugin mapping port", err)
	}
	if err := tx.Commit().Error; err != nil {
		tx.Rollback()
		return util.CreateAPIHandleErrorFromDBError("commit delete err", err)
	}
	return nil
}

//SetTenantServicePluginRelation SetTenantServicePluginRelation
func (s *ServiceAction) SetTenantServicePluginRelation(tenantID, serviceID string, pss *api_model.PluginSetStruct) *util.APIHandleError {
	tx := db.GetManager().Begin()
	plugin, err := db.GetManager().TenantPluginDao().GetPluginByID(pss.Body.PluginID)
	if err != nil {
		tx.Rollback()
		return util.CreateAPIHandleErrorFromDBError("get plugin by plugin id", err)
	}

	catePlugin := strings.Split(plugin.PluginModel, ":")[0]
	//TODO:检查是否存在该大类插件
	crt, err := db.GetManager().TenantServicePluginRelationDao().CheckSomeModelLikePluginByServiceID(
		serviceID,
		catePlugin,
	)
	if err != nil {
		tx.Rollback()
		return util.CreateAPIHandleErrorFromDBError("check plugin model", err)
	}
	if crt {
		tx.Rollback()
		return util.CreateAPIHandleError(400, fmt.Errorf("can not add this kind plugin, a same kind plugin has been linked"))
	}
	if plugin.PluginModel == dbmodel.UpNetPlugin {
		ports, err := db.GetManager().TenantServicesPortDao().GetPortsByServiceID(serviceID)
		if err != nil {
			tx.Rollback()
			return util.CreateAPIHandleErrorFromDBError("get ports by service id", err)
		}
		for _, p := range ports {
			if p.IsInnerService || p.IsOuterService {
				pluginPort, err := db.GetManager().TenantServicesStreamPluginPortDaoTransactions(tx).SetPluginMappingPort(
					tenantID,
					serviceID,
					dbmodel.UpNetPlugin,
					p.ContainerPort,
				)
				if err != nil {
					tx.Rollback()
					logrus.Errorf(fmt.Sprintf("set upstream port %d error, %v", p.ContainerPort, err))
					return util.CreateAPIHandleErrorFromDBError(
						fmt.Sprintf("set upstream port %d error ", p.ContainerPort),
						err,
					)
				}
				logrus.Debugf("set plugin upsteam port %d->%d", p.ContainerPort, pluginPort)
				continue
			}
		}
	}
	relation := &dbmodel.TenantServicePluginRelation{
		VersionID:   pss.Body.VersionID,
		ServiceID:   serviceID,
		PluginID:    pss.Body.PluginID,
		Switch:      pss.Body.Switch,
		PluginModel: plugin.PluginModel,
	}
	if err := db.GetManager().TenantServicePluginRelationDaoTransactions(tx).AddModel(relation); err != nil {
		tx.Rollback()
		return util.CreateAPIHandleErrorFromDBError("set service plugin relation", err)
	}
	if err := tx.Commit().Error; err != nil {
		tx.Rollback()
		return util.CreateAPIHandleErrorFromDBError("commit set service plugin relation", err)
	}
	return nil
}

//UpdateTenantServicePluginRelation UpdateTenantServicePluginRelation
func (s *ServiceAction) UpdateTenantServicePluginRelation(serviceID string, pss *api_model.PluginSetStruct) *util.APIHandleError {
	relation, err := db.GetManager().TenantServicePluginRelationDao().GetRelateionByServiceIDAndPluginID(serviceID, pss.Body.PluginID)
	if err != nil {
		return util.CreateAPIHandleErrorFromDBError("get relation by serviceid and pluginid", err)
	}
	relation.VersionID = pss.Body.VersionID
	relation.Switch = pss.Body.Switch
	err = db.GetManager().TenantServicePluginRelationDao().UpdateModel(relation)
	if err != nil {
		return util.CreateAPIHandleErrorFromDBError("update relation between plugin and service", err)
	}
	return nil
}

//SetVersionEnv SetVersionEnv
func (s *ServiceAction) SetVersionEnv(serviecID, pluginID string, sve *api_model.SetVersionEnv) *util.APIHandleError {
	tx := db.GetManager().Begin()
	for _, env := range sve.Body.Envs {
		tpv := &dbmodel.TenantPluginVersionEnv{
			PluginID:  pluginID,
			ServiceID: serviecID,
			EnvName:   env.EnvName,
			EnvValue:  env.EnvValue,
		}
		if err := db.GetManager().TenantPluginVersionENVDaoTransactions(tx).AddModel(tpv); err != nil {
			tx.Rollback()
			return util.CreateAPIHandleErrorFromDBError("set version env", err)
		}
	}
	if err := tx.Commit().Error; err != nil {
		tx.Rollback()
		return util.CreateAPIHandleErrorFromDBError("commit set version env", err)
	}
	return nil
}

//UpdateVersionEnv UpdateVersionEnv
func (s *ServiceAction) UpdateVersionEnv(serviceID string, uve *api_model.UpdateVersionEnv) *util.APIHandleError {
	env, err := db.GetManager().TenantPluginVersionENVDao().GetVersionEnvByEnvName(serviceID, uve.PluginID, uve.EnvName)
	if err != nil {
		return util.CreateAPIHandleErrorFromDBError("update get version env", err)
	}
	env.EnvValue = uve.Body.EnvValue
	if err := db.GetManager().TenantPluginVersionENVDao().UpdateModel(env); err != nil {
		return util.CreateAPIHandleErrorFromDBError("update version env", err)
	}
	return nil
}

//TransStatus trans service status
func TransStatus(eStatus string) string {
	switch eStatus {
	case "starting":
		return "启动中"
	case "abnormal":
		return "运行异常"
	case "upgrade":
		return "升级中"
	case "closed":
		return "已关闭"
	case "stopping":
		return "关闭中"
	case "checking":
		return "检测中"
	case "unusual":
		return "运行异常"
	case "running":
		return "运行中"
	case "failure":
		return "未知"
	case "undeploy":
		return "未部署"
	case "deployed":
		return "已部署"
	}
	return ""
}

//CheckLabel check label
func CheckLabel(serviceID string) bool {
	//true for v2, false for v1
	serviceLabel, err := db.GetManager().TenantServiceLabelDao().GetTenantServiceLabel(serviceID)
	if err != nil {
		return false
	}
	if serviceLabel != nil && len(serviceLabel) > 0 {
		logrus.Debugf("length serviceLabel, %v, %+v", len(serviceLabel), *(serviceLabel[0]))
		return true
	}
	return false
}

//GetPodList Get pod list
func GetPodList(namespace, serviceAlias string, cli *kubernetes.Clientset) (*v1.PodList, error) {
	labelname := fmt.Sprintf("name=%v", serviceAlias)
	pods, err := cli.CoreV1().Pods(namespace).List(metav1.ListOptions{LabelSelector: labelname})
	if err != nil {
		return nil, err
	}
	return pods, err
}

//CheckMapKey CheckMapKey
func CheckMapKey(rebody map[string]interface{}, key string, defaultValue interface{}) map[string]interface{} {
	if _, ok := rebody[key]; ok {
		return rebody
	}
	rebody[key] = defaultValue
	return rebody
}

func chekeServiceLabel(v string) string {
	if strings.Contains(v, "有状态") {
		return core_util.StatefulServiceType
	}
	if strings.Contains(v, "无状态") {
		return core_util.StatelessServiceType
	}
	return v
}
