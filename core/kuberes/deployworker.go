/*
Copyright 2021 The AtomCI Group Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kuberes

import (
	"fmt"

	"github.com/go-atomci/atomci/core/settings"
	"github.com/go-atomci/atomci/dao"
	"github.com/go-atomci/atomci/middleware/log"
	"github.com/go-atomci/atomci/models"

	"github.com/go-atomci/atomci/utils/query"

	"github.com/astaxie/beego/orm"
)

type ExtensionParam struct {
	Force   bool //when user deploy its app and the app is existed in other namespace, the old app will be deleted
	Patcher PatcherFunction
}

type DeployWorker struct {
	Name      string
	arHandle  *AppRes
	kubeRes   *KubeAppRes
	extension *ExtensionParam
	template  AppTemplate
}

func NewDeployWorker(name, namespace, kind string, ar *AppRes, eparam *ExtensionParam, tpl AppTemplate) *DeployWorker {
	return &DeployWorker{
		Name:      name,
		arHandle:  ar,
		kubeRes:   NewKubeAppRes(ar.Client, ar.Cluster, namespace, kind),
		extension: eparam,
		template:  tpl,
	}
}

func (wk *DeployWorker) Start(templateName string, param AppParam) error {
	log.Log.Info("deploying application: ", wk.Name)
	err := wk.checkAppRes(param.Name)
	if err != nil {
		return err
	}
	app, err := wk.arHandle.Appmodel.GetAppByName(wk.arHandle.Cluster, wk.kubeRes.Namespace, param.Name)
	if err == nil {
		return wk.updateAppRes(*app)
	}
	if err != orm.ErrNoRows {
		return err
	}
	return wk.createAppRes(templateName, param)

}

//check app res, maybe delete some data
func (wk *DeployWorker) checkAppRes(appname string) error {
	// check app name uniqueness in signal cluster
	exoticapps, err := wk.arHandle.Appmodel.GetExoticAppListByName(wk.arHandle.Cluster, wk.kubeRes.Namespace, appname)
	if err != nil {
		return err
	}
	if len(exoticapps) != 0 {
		var exoticns []string
		if wk.extension.Force {
			//check right
			for _, app := range exoticapps {
				exoticns = append(exoticns, app.Namespace)
			}
			if len(exoticns) == 0 {
				//uninstall
				for _, app := range exoticapps {
					log.Log.Warn(fmt.Sprintf("deleting application(%s), cluster(%s), namespace(%s), and you have right to do it...", appname, wk.arHandle.Cluster, app.Namespace))
					if err = wk.arHandle.DeleteApp(app.Namespace, app.Name); err != nil {
						return fmt.Errorf("the application(%s) is existed in namespace %v of cluster %v, and delete old application failed: %s",
							appname, app.Namespace, wk.arHandle.Cluster, err.Error())
					}
					log.Log.Warn(fmt.Sprintf("delete application(%s), cluster(%s), namespace(%s) successfully, and you have right to do it...", appname, wk.arHandle.Cluster, app.Namespace))
				}
			}
		} else {
			//no right
			for _, app := range exoticapps {
				exoticns = append(exoticns, app.Namespace)
			}
		}
		if len(exoticns) != 0 {
			return fmt.Errorf("the application(%s) is existed in namespace %v of cluster %v, and you have no right to cover the old application", appname, exoticns, wk.arHandle.Cluster)
		}
	}

	return nil
}

func (wk *DeployWorker) updateAppRes(app models.CaasApplication) error {
	//delete possible resource
	log.Log.Info("delete possible deploy and pod resource: ", wk.arHandle.Cluster, wk.kubeRes.Namespace, app.Name, app.Kind)
	wk.deleteApplication(app.Name)
	_, err := wk.arHandle.ReconfigureApp(app, wk.template)
	if err != nil {
		return err
	}
	return nil
}

func (wk *DeployWorker) createAppRes(templateName string, param AppParam) error {
	// create new app resource
	app, err := wk.createKubeAppRes(templateName, param)
	if err != nil {
		return err
	}
	err = wk.arHandle.Appmodel.CreateApp(*app)
	if err != nil {
		wk.kubeRes.DeleteAppResource(wk.template)
		wk.arHandle.Appmodel.DeleteApp(*app)
		return err
	}
	if wk.extension != nil {
		if wk.extension.Patcher != nil {
			wk.extension.Patcher(*app)
		}
	}
	return nil
}

func (wk *DeployWorker) createKubeAppRes(templateName string, param AppParam) (*models.CaasApplication, error) {
	app, err := wk.template.GenerateAppObject(wk.arHandle.Cluster, wk.kubeRes.Namespace, templateName, wk.arHandle.ProjectID)
	if err != nil {
		return nil, err
	}
	//delete possible resource
	log.Log.Info("delete possible deploy and pod resource: ", wk.arHandle.Cluster, wk.kubeRes.Namespace, param.Name, app.Kind)
	wk.deleteApplication(param.Name)
	log.Log.Info("create resource: ", wk.arHandle.Cluster, wk.kubeRes.Namespace, param.Name, app.Kind)
	if err := wk.kubeRes.CreateAppResource(wk.template); err != nil {
		return nil, err
	}
	return app, nil
}

func (wk *DeployWorker) deleteApplication(appname string) {
	filter := query.NewFilterQuery(false)
	filter.FilterKey = "name"
	filter.FilterVal = appname
	res, err := wk.arHandle.Appmodel.GetAppList(filter, wk.arHandle.ProjectID, wk.kubeRes.cluster, wk.kubeRes.Namespace)
	if err != nil {
		log.Log.Error("deleteApplication error: ", err.Error())
		return
	}
	applist := res.Item.([]models.CaasApplication)
	ar := *wk.arHandle
	for _, app := range applist {
		exist, err := wk.kubeRes.CheckAppIsExisted(app.Name)
		if err == nil && exist && wk.arHandle.Cluster != app.Cluster {
			ar.Cluster = app.Cluster
			err = (&ar).DeleteApp(app.Namespace, app.Name)
			if err != nil {
				log.Log.Info(fmt.Sprintf("delete unsuitable application(%s/%s) failed: %v!", app.Cluster, app.Name, err))
			} else {
				log.Log.Info(fmt.Sprintf("delete unsuitable application(%s/%s) successfully!", app.Cluster, app.Name))
			}
		}
	}
}

func getDefaultPullSecretAndHarborAddr(envID int64) (string, string, error) {
	projectEnv, err := dao.NewProjectModel().GetProjectEnvByID(envID)
	if err != nil {
		log.Log.Error("when create harbor secret get project env by id: %v, error: %s", envID, err.Error())
		return "", "", err
	}
	integrateSettingHarbor, err := dao.NewSysSettingModel().GetIntegrateSettingByID(projectEnv.Harbor)
	if err != nil {
		log.Log.Error("when create harbor secret get integrate setting by id: %v, error: %s", projectEnv.Harbor, err.Error())
		return "", "", err
	}
	config := settings.Config{}
	configJSON, err := config.Struct(integrateSettingHarbor.Config, integrateSettingHarbor.Type)
	if err != nil {
		log.Log.Error("when parse harbor config error: %s", err.Error())
		return "", "", err
	}
	var url string
	if harborConf, ok := configJSON.(*settings.HarborConfig); ok {
		url = harborConf.URL
	}
	return "harbor-" + integrateSettingHarbor.Name, url, nil
}