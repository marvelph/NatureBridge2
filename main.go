package main

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"log"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/brutella/hc"
	"github.com/brutella/hc/accessory"
	"github.com/brutella/hc/service"
	"github.com/tenntenn/natureremo"
)

const timeout = 10 * time.Second
const interval = 1 * time.Minute

type natureBridge struct {
	*accessory.Accessory
}

func newNatureBridge(id uint64, u *natureremo.User) *natureBridge {
	nb := natureBridge{}
	nb.Accessory = accessory.New(accessory.Info{Name: u.Nickname, ID: id}, accessory.TypeBridge)
	return &nb
}

type deviceUpdater interface {
	update(d *natureremo.Device)
}

type natureRemo struct {
	*accessory.Accessory
	temperatureSensor *service.TemperatureSensor
	humiditySensor    *service.HumiditySensor
	lightSensor       *service.LightSensor
	client            *natureremo.Client
	context           context.Context
	device            *natureremo.Device
}

func newNatureRemo(id uint64, cli *natureremo.Client, ctx context.Context, d *natureremo.Device) *natureRemo {
	nr := natureRemo{}
	nr.client = cli
	nr.context = ctx
	nr.device = d

	nr.Accessory = accessory.New(
		accessory.Info{
			Name:             nr.device.Name,
			Manufacturer:     "Nature",
			FirmwareRevision: nr.device.FirmwareVersion,
			ID:               id,
		},
		accessory.TypeSensor,
	)

	nr.temperatureSensor = service.NewTemperatureSensor()
	if t, ok := nr.toCurrentTemperature(nr.device.NewestEvents[natureremo.SensorTypeTemperature].Value); ok {
		nr.temperatureSensor.CurrentTemperature.SetValue(t)
	}
	nr.AddService(nr.temperatureSensor.Service)

	if e, ok := nr.device.NewestEvents[natureremo.SensorTypeHumidity]; ok {
		nr.humiditySensor = service.NewHumiditySensor()
		if h, ok := nr.toCurrentRelativeHumidity(e.Value); ok {
			nr.humiditySensor.CurrentRelativeHumidity.SetValue(h)
		}
		nr.AddService(nr.humiditySensor.Service)
	}

	if e, ok := nr.device.NewestEvents[natureremo.SensortypeIllumination]; ok {
		nr.lightSensor = service.NewLightSensor()
		if l, ok := nr.toCurrentAmbientLightLevel(e.Value); ok {
			nr.lightSensor.CurrentAmbientLightLevel.SetValue(l)
		}
		nr.AddService(nr.lightSensor.Service)
	}

	return &nr
}

func (nr *natureRemo) update(d *natureremo.Device) {
	nr.device = d

	if t, ok := nr.toCurrentTemperature(nr.device.NewestEvents[natureremo.SensorTypeTemperature].Value); ok {
		nr.temperatureSensor.CurrentTemperature.SetValue(t)
	}

	if e, ok := nr.device.NewestEvents[natureremo.SensorTypeHumidity]; nr.humiditySensor != nil && ok {
		if h, ok := nr.toCurrentRelativeHumidity(e.Value); ok {
			nr.humiditySensor.CurrentRelativeHumidity.SetValue(h)
		}
	}

	if e, ok := nr.device.NewestEvents[natureremo.SensortypeIllumination]; nr.lightSensor != nil && ok {
		if l, ok := nr.toCurrentAmbientLightLevel(e.Value); ok {
			nr.lightSensor.CurrentAmbientLightLevel.SetValue(l)
		}
	}
}

func (nr *natureRemo) toCurrentTemperature(t float64) (float64, bool) {
	if t < 0.0 || 100.0 < t {
		return 0.0, false
	}

	return math.Round(t*10.0) / 10.0, true
}

func (nr *natureRemo) toCurrentRelativeHumidity(h float64) (float64, bool) {
	if h < 0.0 || 100.0 < h {
		return 0.0, false
	}

	return math.Round(h), true
}

func (nr *natureRemo) toCurrentAmbientLightLevel(l float64) (float64, bool) {
	if l < 0.0001 || 100000 < l {
		return 0.0, false
	}

	return l, true
}

type applianceUpdater interface {
	update(d *natureremo.Device, a *natureremo.Appliance)
}

type airConAppliance struct {
	*accessory.Accessory
	thermostat *service.Thermostat
	client     *natureremo.Client
	context    context.Context
	device     *natureremo.Device
	appliance  *natureremo.Appliance
}

func newAirConAppliance(id uint64, cli *natureremo.Client, ctx context.Context, d *natureremo.Device, a *natureremo.Appliance) *airConAppliance {
	aa := airConAppliance{}
	aa.client = cli
	aa.context = ctx
	aa.device = d
	aa.appliance = a

	aa.Accessory = accessory.New(
		accessory.Info{
			Name:         aa.appliance.Nickname,
			Manufacturer: aa.appliance.Model.Manufacturer,
			Model:        aa.appliance.Model.Name,
			ID:           id,
		},
		accessory.TypeAirConditioner,
	)

	aa.thermostat = service.NewThermostat()
	// エアコンの現在のモードを取得する方法は無いのでリモコンに対する最後の操作を現在のモードと見做す。
	if s, ok := aa.toCurrentHeatingCoolingState(aa.appliance.AirConSettings.OperationMode, aa.appliance.AirConSettings.Button); ok {
		aa.thermostat.CurrentHeatingCoolingState.SetValue(s)
	}
	if s, ok := aa.toTargetHeatingCoolingState(aa.appliance.AirConSettings.OperationMode, aa.appliance.AirConSettings.Button); ok {
		aa.thermostat.TargetHeatingCoolingState.SetValue(s)
	}
	aa.thermostat.TargetHeatingCoolingState.OnValueRemoteUpdate(aa.changeTargetHeatingCoolingState)
	// エアコンの温度計の値を取得する方法は無いのでNatureRemoの温度計の値をエアコンの温度と見做す。
	if t, ok := aa.toCurrentTemperature(aa.device.NewestEvents[natureremo.SensorTypeTemperature].Value); ok {
		aa.thermostat.CurrentTemperature.SetValue(t)
	}
	if t, ok := aa.toTargetTemperature(aa.appliance.AirConSettings.Temperature); ok {
		aa.thermostat.TargetTemperature.SetValue(t)
	}
	aa.thermostat.TargetTemperature.OnValueRemoteUpdate(aa.changeTargetTemperature)
	if u, ok := aa.toTemperatureDisplayUnits(aa.appliance.AirCon.TemperatureUnit); ok {
		aa.thermostat.TemperatureDisplayUnits.SetValue(u)
	}
	// エアコンの表示単位を変更する方法は無いので書き込みには対応できない。
	aa.AddService(aa.thermostat.Service)

	return &aa
}

func (aa *airConAppliance) update(d *natureremo.Device, a *natureremo.Appliance) {
	aa.device = d
	aa.appliance = a

	// エアコンの現在のモードを取得する方法は無いのでリモコンに対する最後の操作を現在のモードと見做す。
	if s, ok := aa.toCurrentHeatingCoolingState(aa.appliance.AirConSettings.OperationMode, aa.appliance.AirConSettings.Button); ok {
		aa.thermostat.CurrentHeatingCoolingState.SetValue(s)
	}
	if s, ok := aa.toTargetHeatingCoolingState(aa.appliance.AirConSettings.OperationMode, aa.appliance.AirConSettings.Button); ok {
		aa.thermostat.TargetHeatingCoolingState.SetValue(s)
	}
	// エアコンの温度計の値を取得する方法は無いのでNatureRemoの温度計の値をエアコンの温度と見做す。
	if t, ok := aa.toCurrentTemperature(aa.device.NewestEvents[natureremo.SensorTypeTemperature].Value); ok {
		aa.thermostat.CurrentTemperature.SetValue(t)
	}
	if t, ok := aa.toTargetTemperature(aa.appliance.AirConSettings.Temperature); ok {
		aa.thermostat.TargetTemperature.SetValue(t)
	}
	if u, ok := aa.toTemperatureDisplayUnits(aa.appliance.AirCon.TemperatureUnit); ok {
		aa.thermostat.TemperatureDisplayUnits.SetValue(u)
	}
}

func (aa *airConAppliance) changeTargetHeatingCoolingState(s int) {
	ctx, cancel := context.WithTimeout(aa.context, timeout)
	defer cancel()

	if m, b, ok := aa.toOperationModeAndButton(s); ok {
		aa.appliance.AirConSettings.OperationMode = m
		aa.appliance.AirConSettings.Button = b
		err := aa.client.ApplianceService.UpdateAirConSettings(ctx, aa.appliance, aa.appliance.AirConSettings)
		if err != nil {
			log.Print(err)
		}
	}
}

func (aa *airConAppliance) changeTargetTemperature(t float64) {
	ctx, cancel := context.WithTimeout(aa.context, timeout)
	defer cancel()

	if t, ok := aa.toTemperature(t); ok {
		aa.appliance.AirConSettings.Temperature = t
		err := aa.client.ApplianceService.UpdateAirConSettings(ctx, aa.appliance, aa.appliance.AirConSettings)
		if err != nil {
			log.Print(err)
		}
	}
}

func (aa *airConAppliance) toCurrentHeatingCoolingState(m natureremo.OperationMode, b natureremo.Button) (int, bool) {
	switch b {
	case natureremo.ButtonPowerOn:
		switch m {
		case natureremo.OperationModeAuto:
			// 自動運転に対する現在のモードを判定する方法はない。
			return 0, false
		case natureremo.OperationModeCool:
			return 2, true
		case natureremo.OperationModeWarm:
			return 1, true
		case natureremo.OperationModeDry:
			// モードが除湿の場合は処理できない。
			return 0, false
		case natureremo.OperationModeBlow:
			// モードが送風の場合は処理できない。
			return 0, false
		}
	case natureremo.ButtonPowerOff:
		return 0, true
	}
	return 0, false // ここに到達する事はない。
}

func (aa *airConAppliance) toTargetHeatingCoolingState(m natureremo.OperationMode, b natureremo.Button) (int, bool) {
	switch b {
	case natureremo.ButtonPowerOn:
		switch m {
		case natureremo.OperationModeAuto:
			return 3, true
		case natureremo.OperationModeCool:
			return 2, true
		case natureremo.OperationModeWarm:
			return 1, true
		case natureremo.OperationModeDry:
			// モードが除湿の場合は処理できない。
			return 0, false
		case natureremo.OperationModeBlow:
			// モードが送風の場合は処理できない。
			return 0, false
		}
	case natureremo.ButtonPowerOff:
		return 0, true
	}
	return 0, false // ここに到達する事はない。
}

func (aa *airConAppliance) toOperationModeAndButton(s int) (natureremo.OperationMode, natureremo.Button, bool) {
	// TODO: エアコンは設定可能なモードの一覧を提供しているのでその値を外れる場合は失敗させる必要がある。
	switch s {
	case 0:
		// 電源を切る場合は現在のモードを維持する。
		return aa.appliance.AirConSettings.OperationMode, natureremo.ButtonPowerOff, false
	case 1:
		return natureremo.OperationModeWarm, natureremo.ButtonPowerOn, true
	case 2:
		return natureremo.OperationModeCool, natureremo.ButtonPowerOn, true
	case 3:
		return natureremo.OperationModeAuto, natureremo.ButtonPowerOn, true
	}
	return "", "", false
}

func (aa *airConAppliance) toCurrentTemperature(t float64) (float64, bool) {
	if t < 0.0 || 100.0 < t {
		return 0.0, false
	}

	return math.Round(t*10.0) / 10.0, true
}

func (aa *airConAppliance) toTargetTemperature(t string) (float64, bool) {
	v, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0.0, false
	}

	if v < 10.0 || 38.0 < v {
		return 0.0, false
	}

	switch aa.appliance.AirCon.TemperatureUnit {
	case natureremo.TemperatureUnitAuto:
		// 温度の単位が自動の場合は処理できない。
		return 0.0, false
	case natureremo.TemperatureUnitFahrenheit:
		return math.Round((v-32.0)*5.0/9.0*10.0) / 10.0, true
	case natureremo.TemperatureUnitCelsius:
		return math.Round(v*10.0) / 10.0, true
	}
	return 0.0, false // ここに到達する事はない。
}

func (aa *airConAppliance) toTemperature(t float64) (string, bool) {
	switch aa.appliance.AirCon.TemperatureUnit {
	case natureremo.TemperatureUnitAuto:
		// 温度の単位が自動の場合は処理できない。
		return "", false
	case natureremo.TemperatureUnitFahrenheit:
		t = t*9.0/5.0 + 32.0
	}

	// TODO: エアコンは設定可能な温度の一覧を提供しているのでその値を外れる場合は失敗させる必要がある。
	return strconv.FormatFloat(t, 'f', 1, 64), true
}

func (aa *airConAppliance) toTemperatureDisplayUnits(u natureremo.TemperatureUnit) (int, bool) {
	switch u {
	case natureremo.TemperatureUnitAuto:
		// 温度の単位が自動の場合は処理できない。
		return 0, false
	case natureremo.TemperatureUnitFahrenheit:
		return 1, true
	case natureremo.TemperatureUnitCelsius:
		return 0, true
	}
	return 0, false // ここに到達する事はない。
}

type lightAppliance struct {
	*accessory.Accessory
	lightbulb *service.Lightbulb
	client    *natureremo.Client
	context   context.Context
	device    *natureremo.Device
	appliance *natureremo.Appliance
}

func newLightAppliance(id uint64, cli *natureremo.Client, ctx context.Context, d *natureremo.Device, a *natureremo.Appliance) *lightAppliance {
	la := lightAppliance{}
	la.client = cli
	la.context = ctx
	la.device = d
	la.appliance = a

	la.Accessory = accessory.New(
		accessory.Info{
			Name:         la.appliance.Nickname,
			Manufacturer: la.appliance.Model.Manufacturer,
			Model:        la.appliance.Model.Name,
			ID:           id,
		},
		accessory.TypeLightbulb,
	)

	la.lightbulb = service.NewLightbulb()
	if o, ok := la.toOn(la.appliance.Light.State.Power); ok {
		la.lightbulb.On.SetValue(o)
	}
	la.lightbulb.On.OnValueRemoteUpdate(la.changeOn)
	la.AddService(la.lightbulb.Service)

	return &la
}

func (la *lightAppliance) update(d *natureremo.Device, a *natureremo.Appliance) {
	la.device = d
	la.appliance = a

	if o, ok := la.toOn(la.appliance.Light.State.Power); ok {
		la.lightbulb.On.SetValue(o)
	}
}

func (la *lightAppliance) changeOn(o bool) {
	ctx, cancel := context.WithTimeout(la.context, timeout)
	defer cancel()

	_, err := la.client.ApplianceService.SendLightSignal(ctx, la.appliance, la.toPower(o))
	if err != nil {
		log.Print(err)
	}
}

func (la *lightAppliance) toOn(o string) (bool, bool) {
	switch o {
	case "on":
		return true, true
	case "off":
		return false, true
	}
	return false, false
}

func (la *lightAppliance) toPower(o bool) string {
	if o {
		return "on"
	} else {
		return "off"
	}
}

type application struct {
	client     *natureremo.Client
	context    context.Context
	user       *natureremo.User
	transport  hc.Transport
	devices    map[string]deviceUpdater
	appliances map[string]applianceUpdater
	aids       map[string]uint64
}

func newApplication(ctx context.Context) *application {
	app := application{}
	app.client = natureremo.NewClient(os.Getenv("ACCESS_TOKEN"))
	app.context = ctx
	app.devices = make(map[string]deviceUpdater)
	app.appliances = make(map[string]applianceUpdater)
	app.aids = make(map[string]uint64)
	return &app
}

func (app *application) stop() {
	if app.transport != nil {
		<-app.transport.Stop()
		app.transport = nil
	}
}

func (app *application) update() error {
	ds, as, err := app.getAll()
	if err != nil {
		return err
	}

	if app.wasChanged(ds, as) {
		err := app.build(ds, as)
		if err != nil {
			return err
		}
	} else {
		app.apply(ds, as)
	}
	return nil
}

func (app *application) getAll() ([]*natureremo.Device, []*natureremo.Appliance, error) {
	if app.user == nil {
		uctx, cancel := context.WithTimeout(app.context, timeout)
		defer cancel()

		u, err := app.client.UserService.Me(uctx)
		if err != nil {
			return nil, nil, err
		}
		app.user = u
	}

	dctx, cancel := context.WithTimeout(app.context, timeout)
	defer cancel()

	ds, err := app.client.DeviceService.GetAll(dctx)
	if err != nil {
		return nil, nil, err
	}

	actx, cancel := context.WithTimeout(app.context, timeout)
	defer cancel()

	as, err := app.client.ApplianceService.GetAll(actx)
	if err != nil {
		return nil, nil, err
	}

	return ds, as, nil
}

func (app *application) wasChanged(ds []*natureremo.Device, as []*natureremo.Appliance) bool {
	ts := make([]*natureremo.Appliance, 0, len(as))
	for _, a := range as {
		switch a.Type {
		case natureremo.ApplianceTypeAirCon:
			ts = append(ts, a)
		case natureremo.ApplianceTypeLight:
			ts = append(ts, a)
		}
	}

	if len(app.devices) != len(ds) || len(app.appliances) != len(ts) {
		return true
	}

	for _, d := range ds {
		if _, ok := app.devices[d.ID]; !ok {
			return true
		}
	}

	for _, a := range ts {
		if _, ok := app.appliances[a.ID]; !ok {
			return true
		}
	}

	return false
}

func (app *application) build(ds []*natureremo.Device, as []*natureremo.Appliance) error {
	err := app.loadAids()
	if err != nil {
		return err
	}

	if app.transport != nil {
		<-app.transport.Stop()
		app.transport = nil
	}

	app.devices = make(map[string]deviceUpdater, len(ds))
	app.appliances = make(map[string]applianceUpdater, len(as))

	nb := newNatureBridge(app.getAid(app.user.ID), app.user)
	accs := make([]*accessory.Accessory, 0, len(app.devices)+len(app.appliances))
	ts := make(map[string]*natureremo.Device, len(ds))

	for _, d := range ds {
		nr := newNatureRemo(app.getAid(d.ID), app.client, app.context, d)
		app.devices[d.ID] = nr
		accs = append(accs, nr.Accessory)
		ts[d.ID] = d
	}

	for _, a := range as {
		switch a.Type {
		case natureremo.ApplianceTypeAirCon:
			aa := newAirConAppliance(app.getAid(a.ID), app.client, app.context, ts[a.Device.ID], a)
			app.appliances[a.ID] = aa
			accs = append(accs, aa.Accessory)
		case natureremo.ApplianceTypeLight:
			la := newLightAppliance(app.getAid(a.ID), app.client, app.context, ts[a.Device.ID], a)
			app.appliances[a.ID] = la
			accs = append(accs, la.Accessory)
		}
	}

	err = app.saveAids()
	if err != nil {
		return err
	}

	c := hc.Config{Pin: os.Getenv("PIN")}
	t, err := hc.NewIPTransport(c, nb.Accessory, accs...)
	if err != nil {
		return err
	}

	app.transport = t
	go func() {
		app.transport.Start()
	}()

	return nil
}

func (app *application) apply(ds []*natureremo.Device, as []*natureremo.Appliance) {
	ts := make(map[string]*natureremo.Device, len(ds))

	for _, d := range ds {
		app.devices[d.ID].update(d)
		ts[d.ID] = d
	}

	for _, a := range as {
		if v, ok := app.appliances[a.ID]; ok {
			v.update(ts[a.Device.ID], a)
		}
	}
}

func (app *application) getAid(id string) uint64 {
	aid, ok := app.aids[id]
	if ok {
		return aid
	}

	aid = uint64(len(app.aids) + 1)
	app.aids[id] = aid
	return aid
}

func (app *application) saveAids() error {
	j, err := json.Marshal(app.aids)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(filepath.Join(os.Getenv("DATA_DIRECTORY"), "aids.json"), j, 0644)
	if err != nil {
		return err
	}

	return nil
}

func (app *application) loadAids() error {
	bs, err := ioutil.ReadFile(filepath.Join(os.Getenv("DATA_DIRECTORY"), "aids.json"))
	if err != nil {
		return err
	}

	err = json.Unmarshal(bs, &app.aids)
	if err != nil {
		return err
	}

	return nil
}

func mainHandler(ctx context.Context) {
	app := newApplication(ctx)
	defer app.stop()

	err := app.update()
	if err != nil {
		log.Print(err)
	}

	tkr := time.NewTicker(interval)
	for {
		select {
		case <-tkr.C:
			err := app.update()
			if err != nil {
				log.Print(err)
			}
		case <-ctx.Done():
			return
		}
	}
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sc)
	go func() {
		defer cancel()
		<-sc
	}()

	mainHandler(ctx)
}
