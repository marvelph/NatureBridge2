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

func newNatureBridge(id uint64, usr *natureremo.User) *natureBridge {
	bri := natureBridge{}
	bri.Accessory = accessory.New(accessory.Info{Name: usr.Nickname, ID: id}, accessory.TypeBridge)
	return &bri
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

func newNatureRemo(id uint64, cli *natureremo.Client, ctx context.Context, dev *natureremo.Device) *natureRemo {
	rem := natureRemo{}
	rem.client = cli
	rem.context = ctx
	rem.device = dev

	rem.Accessory = accessory.New(
		accessory.Info{
			Name:             rem.device.Name,
			Manufacturer:     "Nature",
			FirmwareRevision: rem.device.FirmwareVersion,
			ID:               id,
		},
		accessory.TypeSensor,
	)

	rem.temperatureSensor = service.NewTemperatureSensor()
	if tmp, ok := rem.getCurrentTemperature(); ok {
		rem.temperatureSensor.CurrentTemperature.SetValue(tmp)
	}
	rem.AddService(rem.temperatureSensor.Service)

	if _, ok := rem.device.NewestEvents[natureremo.SensorTypeHumidity]; ok {
		rem.humiditySensor = service.NewHumiditySensor()
		if hum, ok := rem.getCurrentRelativeHumidity(); ok {
			rem.humiditySensor.CurrentRelativeHumidity.SetValue(hum)
		}
		rem.AddService(rem.humiditySensor.Service)
	}

	if _, ok := rem.device.NewestEvents[natureremo.SensortypeIllumination]; ok {
		rem.lightSensor = service.NewLightSensor()
		if ill, ok := rem.getCurrentAmbientLightLevel(); ok {
			rem.lightSensor.CurrentAmbientLightLevel.SetValue(ill)
		}
		rem.AddService(rem.lightSensor.Service)
	}

	return &rem
}

func (rem *natureRemo) update(dev *natureremo.Device) {
	rem.device = dev

	if tmp, ok := rem.getCurrentTemperature(); ok {
		rem.temperatureSensor.CurrentTemperature.SetValue(tmp)
	}

	if rem.humiditySensor != nil {
		if hum, ok := rem.getCurrentRelativeHumidity(); ok {
			rem.humiditySensor.CurrentRelativeHumidity.SetValue(hum)
		}
	}

	if rem.lightSensor != nil {
		if ill, ok := rem.getCurrentAmbientLightLevel(); ok {
			rem.lightSensor.CurrentAmbientLightLevel.SetValue(ill)
		}
	}
}

func (rem *natureRemo) getCurrentTemperature() (float64, bool) {
	if evt, ok := rem.device.NewestEvents[natureremo.SensorTypeTemperature]; ok {
		tmp := evt.Value

		if tmp < 0.0 || 100.0 < tmp {
			return 0.0, false
		}

		return math.Round(tmp*10.0) / 10.0, true
	}

	return 0.0, false
}

func (rem *natureRemo) getCurrentRelativeHumidity() (float64, bool) {
	if evt, ok := rem.device.NewestEvents[natureremo.SensorTypeHumidity]; ok {
		hum := evt.Value

		if hum < 0.0 || 100.0 < hum {
			return 0.0, false
		}

		return math.Round(hum), true
	}

	return 0.0, false
}

func (rem *natureRemo) getCurrentAmbientLightLevel() (float64, bool) {
	if evt, ok := rem.device.NewestEvents[natureremo.SensortypeIllumination]; ok {
		ill := evt.Value

		if ill < 0.0001 || 100000.0 < ill {
			return 0.0, false
		}

		return ill, true
	}

	return 0.0, false
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

func newAirConAppliance(id uint64, cli *natureremo.Client, ctx context.Context, dev *natureremo.Device, ali *natureremo.Appliance) *airConAppliance {
	air := airConAppliance{}
	air.client = cli
	air.context = ctx
	air.device = dev
	air.appliance = ali

	air.Accessory = accessory.New(
		accessory.Info{
			Name:         air.appliance.Nickname,
			Manufacturer: air.appliance.Model.Manufacturer,
			Model:        air.appliance.Model.Name,
			ID:           id,
		},
		accessory.TypeAirConditioner,
	)

	air.thermostat = service.NewThermostat()
	if sta, ok := air.getCurrentHeatingCoolingState(); ok {
		air.thermostat.CurrentHeatingCoolingState.SetValue(sta)
	}

	if sta, ok := air.getTargetHeatingCoolingState(); ok {
		air.thermostat.TargetHeatingCoolingState.SetValue(sta)
	}

	air.thermostat.TargetHeatingCoolingState.OnValueRemoteUpdate(air.changeTargetHeatingCoolingState)
	if tmp, ok := air.getCurrentTemperature(); ok {
		air.thermostat.CurrentTemperature.SetValue(tmp)
	}

	if tmp, ok := air.getTargetTemperature(); ok {
		air.thermostat.TargetTemperature.SetValue(tmp)
	}
	min, max, stp := air.getTargetTemperatureMinAndMaxAndStep()
	air.thermostat.TargetTemperature.SetMinValue(min)
	air.thermostat.TargetTemperature.SetMaxValue(max)
	air.thermostat.TargetTemperature.SetStepValue(stp)
	air.thermostat.TargetTemperature.OnValueRemoteUpdate(air.changeTargetTemperature)

	if uni, ok := air.getTemperatureDisplayUnits(); ok {
		air.thermostat.TemperatureDisplayUnits.SetValue(uni)
	}
	// エアコンの表示単位を変更する方法は無いので書き込みには対応できない。
	air.AddService(air.thermostat.Service)

	return &air
}

func (air *airConAppliance) update(dev *natureremo.Device, ali *natureremo.Appliance) {
	air.device = dev
	air.appliance = ali

	if sta, ok := air.getCurrentHeatingCoolingState(); ok {
		air.thermostat.CurrentHeatingCoolingState.SetValue(sta)
	}
	if sta, ok := air.getTargetHeatingCoolingState(); ok {
		air.thermostat.TargetHeatingCoolingState.SetValue(sta)
	}
	if tmp, ok := air.getCurrentTemperature(); ok {
		air.thermostat.CurrentTemperature.SetValue(tmp)
	}
	if tmp, ok := air.getTargetTemperature(); ok {
		air.thermostat.TargetTemperature.SetValue(tmp)
	}
}

func (air *airConAppliance) changeTargetHeatingCoolingState(sta int) {
	ctx, cancel := context.WithTimeout(air.context, timeout)
	defer cancel()

	if mod, bot, ok := air.convertOperationModeAndButton(sta); ok {
		air.appliance.AirConSettings.OperationMode = mod
		air.appliance.AirConSettings.Button = bot
		err := air.client.ApplianceService.UpdateAirConSettings(ctx, air.appliance, air.appliance.AirConSettings)
		if err != nil {
			log.Print(err)
		}
	}
}

func (air *airConAppliance) changeTargetTemperature(tmp float64) {
	ctx, cancel := context.WithTimeout(air.context, timeout)
	defer cancel()

	if t, ok := air.convertTemperature(tmp); ok {
		air.appliance.AirConSettings.Temperature = t
		err := air.client.ApplianceService.UpdateAirConSettings(ctx, air.appliance, air.appliance.AirConSettings)
		if err != nil {
			log.Print(err)
		}
	}
}

func (air *airConAppliance) getCurrentHeatingCoolingState() (int, bool) {
	// エアコン本体の現在のモードを取得する方法は無いのでリモコンに対する最後の操作を現在のモードと見做す。
	switch air.appliance.AirConSettings.Button {
	case natureremo.ButtonPowerOn:
		switch air.appliance.AirConSettings.OperationMode {
		case natureremo.OperationModeAuto:
			// 自動運転に対する現在のモードを推定する方法はない。
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

	return 0, false
}

func (air *airConAppliance) getTargetHeatingCoolingState() (int, bool) {
	switch air.appliance.AirConSettings.Button {
	case natureremo.ButtonPowerOn:
		switch air.appliance.AirConSettings.OperationMode {
		case natureremo.OperationModeAuto:
			return 3, true
		case natureremo.OperationModeCool:
			return 2, true
		case natureremo.OperationModeWarm:
			return 1, true
		case natureremo.OperationModeDry:
			// モードが除湿の場合は設定できない。
			return 0, false
		case natureremo.OperationModeBlow:
			// モードが送風の場合は設定できない。
			return 0, false
		}
	case natureremo.ButtonPowerOff:
		return 0, true
	}
	return 0, false
}

func (air *airConAppliance) convertOperationModeAndButton(sta int) (natureremo.OperationMode, natureremo.Button, bool) {
	switch sta {
	case 0:
		// 電源を切る場合は現在のモードを維持する。
		return air.appliance.AirConSettings.OperationMode, natureremo.ButtonPowerOff, true
	case 1:
		if _, ok := air.appliance.AirCon.Range.Modes[natureremo.OperationModeWarm]; ok {
			return natureremo.OperationModeWarm, natureremo.ButtonPowerOn, true
		}
	case 2:
		if _, ok := air.appliance.AirCon.Range.Modes[natureremo.OperationModeCool]; ok {
			return natureremo.OperationModeCool, natureremo.ButtonPowerOn, true
		}
	case 3:
		if _, ok := air.appliance.AirCon.Range.Modes[natureremo.OperationModeAuto]; ok {
			return natureremo.OperationModeAuto, natureremo.ButtonPowerOn, true
		}
	}
	return "", "", false
}

func (air *airConAppliance) getCurrentTemperature() (float64, bool) {
	// エアコン本体の温度計の値を取得する方法は無いのでNatureRemoの温度計の値をエアコンの温度と見做す。
	if evt, ok := air.device.NewestEvents[natureremo.SensorTypeTemperature]; ok {
		tmp := evt.Value

		if tmp < 0.0 || 100.0 < tmp {
			return 0.0, false
		}

		return math.Round(tmp*10.0) / 10.0, true
	}

	return 0.0, false
}

func (air *airConAppliance) getTargetTemperature() (float64, bool) {
	tmp, err := strconv.ParseFloat(air.appliance.AirConSettings.Temperature, 64)
	if err != nil {
		return 0.0, false
	}

	if tmp < 10.0 || 38.0 < tmp {
		return 0.0, false
	}

	return math.Round(tmp*10.0) / 10.0, true
}

func (air *airConAppliance) getTargetTemperatureMinAndMaxAndStep() (float64, float64, float64) {
	min := 100.0
	max := 0.0
	stp := 1.0

	for _, rng := range air.appliance.AirCon.Range.Modes {
		for _, v := range rng.Temperature {
			t, err := strconv.ParseFloat(v, 64)
			if err != nil {
				continue
			}

			min = math.Min(t, min)
			max = math.Max(t, max)
		}
	}

	min = math.Max(10.0, min)
	max = math.Min(38.0, max)
	return min, max, stp
}

func (air *airConAppliance) convertTemperature(tmp float64) (string, bool) {
	if rng, ok := air.appliance.AirCon.Range.Modes[air.appliance.AirConSettings.OperationMode]; ok {
		for _, v := range rng.Temperature {
			t, err := strconv.ParseFloat(v, 64)
			if err != nil {
				continue
			}

			if t == tmp {
				return v, true
			}
		}
	}

	return "", false
}

func (air *airConAppliance) getTemperatureDisplayUnits() (int, bool) {
	switch air.appliance.AirCon.TemperatureUnit {
	case natureremo.TemperatureUnitAuto:
		// 温度の単位が自動の場合は設定できない。
		return 0, false
	case natureremo.TemperatureUnitFahrenheit:
		return 1, true
	case natureremo.TemperatureUnitCelsius:
		return 0, true
	}
	return 0, false
}

type lightAppliance struct {
	*accessory.Accessory
	lightbulb *service.Lightbulb
	client    *natureremo.Client
	context   context.Context
	device    *natureremo.Device
	appliance *natureremo.Appliance
}

func newLightAppliance(id uint64, cli *natureremo.Client, ctx context.Context, dev *natureremo.Device, ali *natureremo.Appliance) *lightAppliance {
	lig := lightAppliance{}
	lig.client = cli
	lig.context = ctx
	lig.device = dev
	lig.appliance = ali

	lig.Accessory = accessory.New(
		accessory.Info{
			Name:         lig.appliance.Nickname,
			Manufacturer: lig.appliance.Model.Manufacturer,
			Model:        lig.appliance.Model.Name,
			ID:           id,
		},
		accessory.TypeLightbulb,
	)

	lig.lightbulb = service.NewLightbulb()
	if on, ok := lig.getOn(); ok {
		lig.lightbulb.On.SetValue(on)
	}
	lig.lightbulb.On.OnValueRemoteUpdate(lig.changeOn)
	lig.AddService(lig.lightbulb.Service)

	return &lig
}

func (lig *lightAppliance) update(dev *natureremo.Device, ali *natureremo.Appliance) {
	lig.device = dev
	lig.appliance = ali

	if on, ok := lig.getOn(); ok {
		lig.lightbulb.On.SetValue(on)
	}
}

func (lig *lightAppliance) changeOn(on bool) {
	ctx, cancel := context.WithTimeout(lig.context, timeout)
	defer cancel()

	_, err := lig.client.ApplianceService.SendLightSignal(ctx, lig.appliance, lig.convertPower(on))
	if err != nil {
		log.Print(err)
	}
}

func (lig *lightAppliance) getOn() (bool, bool) {
	switch lig.appliance.Light.State.Power {
	case "on":
		return true, true
	case "off":
		return false, true
	}

	return false, false
}

func (lig *lightAppliance) convertPower(on bool) string {
	if on {
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
	devs, alis, err := app.getAll()
	if err != nil {
		return err
	}

	if app.wasChanged(devs, alis) {
		err := app.build(devs, alis)
		if err != nil {
			return err
		}
	} else {
		app.apply(devs, alis)
	}

	return nil
}

func (app *application) getAll() ([]*natureremo.Device, []*natureremo.Appliance, error) {
	if app.user == nil {
		uctx, cancel := context.WithTimeout(app.context, timeout)
		defer cancel()

		usr, err := app.client.UserService.Me(uctx)
		if err != nil {
			return nil, nil, err
		}
		app.user = usr
	}

	dctx, cancel := context.WithTimeout(app.context, timeout)
	defer cancel()

	devs, err := app.client.DeviceService.GetAll(dctx)
	if err != nil {
		return nil, nil, err
	}

	actx, cancel := context.WithTimeout(app.context, timeout)
	defer cancel()

	alis, err := app.client.ApplianceService.GetAll(actx)
	if err != nil {
		return nil, nil, err
	}

	return devs, alis, nil
}

func (app *application) wasChanged(devs []*natureremo.Device, alis []*natureremo.Appliance) bool {
	ealis := make([]*natureremo.Appliance, 0, len(alis))
	for _, ali := range alis {
		switch ali.Type {
		case natureremo.ApplianceTypeAirCon:
			ealis = append(ealis, ali)
		case natureremo.ApplianceTypeLight:
			ealis = append(ealis, ali)
		}
	}

	if len(app.devices) != len(devs) || len(app.appliances) != len(ealis) {
		return true
	}

	for _, dev := range devs {
		if _, ok := app.devices[dev.ID]; !ok {
			return true
		}
	}

	for _, ali := range ealis {
		if _, ok := app.appliances[ali.ID]; !ok {
			return true
		}
	}

	return false
}

func (app *application) build(devs []*natureremo.Device, alis []*natureremo.Appliance) error {
	err := app.loadAids()
	if err != nil {
		return err
	}

	if app.transport != nil {
		<-app.transport.Stop()
		app.transport = nil
	}

	app.devices = make(map[string]deviceUpdater, len(devs))
	app.appliances = make(map[string]applianceUpdater, len(alis))

	bri := newNatureBridge(app.getAid(app.user.ID), app.user)
	accs := make([]*accessory.Accessory, 0, len(app.devices)+len(app.appliances))
	devm := make(map[string]*natureremo.Device, len(devs))

	for _, dev := range devs {
		rem := newNatureRemo(app.getAid(dev.ID), app.client, app.context, dev)
		app.devices[dev.ID] = rem
		accs = append(accs, rem.Accessory)
		devm[dev.ID] = dev
	}

	for _, ali := range alis {
		switch ali.Type {
		case natureremo.ApplianceTypeAirCon:
			air := newAirConAppliance(app.getAid(ali.ID), app.client, app.context, devm[ali.Device.ID], ali)
			app.appliances[ali.ID] = air
			accs = append(accs, air.Accessory)
		case natureremo.ApplianceTypeLight:
			lig := newLightAppliance(app.getAid(ali.ID), app.client, app.context, devm[ali.Device.ID], ali)
			app.appliances[ali.ID] = lig
			accs = append(accs, lig.Accessory)
		}
	}

	err = app.saveAids()
	if err != nil {
		return err
	}

	con := hc.Config{Pin: os.Getenv("PIN")}
	tra, err := hc.NewIPTransport(con, bri.Accessory, accs...)
	if err != nil {
		return err
	}

	app.transport = tra
	go func() {
		app.transport.Start()
	}()

	return nil
}

func (app *application) apply(devs []*natureremo.Device, alis []*natureremo.Appliance) {
	devm := make(map[string]*natureremo.Device, len(devs))

	for _, dev := range devs {
		app.devices[dev.ID].update(dev)
		devm[dev.ID] = dev
	}

	for _, ali := range alis {
		if aupd, ok := app.appliances[ali.ID]; ok {
			aupd.update(devm[ali.Device.ID], ali)
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

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)
	go func() {
		defer cancel()
		<-sig
	}()

	mainHandler(ctx)
}
