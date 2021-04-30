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

const (
	timeout  = 10 * time.Second
	interval = 1 * time.Minute
)

const version = "0.1"

type Bridge struct {
	*accessory.Accessory
}

func NewBridge(aid uint64, u *natureremo.User) *Bridge {
	br := Bridge{}
	br.Accessory = accessory.New(
		accessory.Info{
			Name:             u.Nickname,
			Manufacturer:     "Kenji Nishishiro",
			Model:            "NatureBridge2",
			FirmwareRevision: version,
			ID:               aid,
		},
		accessory.TypeBridge,
	)
	return &br
}

type RemoUpdater interface {
	Update(d *natureremo.Device)
}

type Remo struct {
	*accessory.Accessory
	temperatureSensor *service.TemperatureSensor
	humiditySensor    *service.HumiditySensor
	lightSensor       *service.LightSensor
	client            *natureremo.Client
	context           context.Context
	device            *natureremo.Device
}

func NewRemo(aid uint64, client *natureremo.Client, ctx context.Context, d *natureremo.Device) *Remo {
	re := Remo{}
	re.Accessory = accessory.New(
		accessory.Info{
			Name:             d.Name,
			Manufacturer:     "Nature Inc.",
			Model:            "Nature Remo", // There is no way to identify the model name.
			FirmwareRevision: d.FirmwareVersion,
			ID:               aid,
		},
		accessory.TypeSensor,
	)

	re.context = ctx
	re.client = client
	re.device = d

	re.temperatureSensor = service.NewTemperatureSensor()
	if t, ok := re.currentTemperature(); ok {
		re.temperatureSensor.CurrentTemperature.SetValue(t)
	}
	re.AddService(re.temperatureSensor.Service)

	if _, ok := re.device.NewestEvents[natureremo.SensorTypeHumidity]; ok {
		re.humiditySensor = service.NewHumiditySensor()
		if h, ok := re.currentRelativeHumidity(); ok {
			re.humiditySensor.CurrentRelativeHumidity.SetValue(h)
		}
		re.AddService(re.humiditySensor.Service)
	}

	if _, ok := re.device.NewestEvents[natureremo.SensortypeIllumination]; ok {
		re.lightSensor = service.NewLightSensor()
		if l, ok := re.currentAmbientLightLevel(); ok {
			re.lightSensor.CurrentAmbientLightLevel.SetValue(l)
		}
		re.AddService(re.lightSensor.Service)
	}

	return &re
}

func (re *Remo) Update(d *natureremo.Device) {
	re.device = d

	if t, ok := re.currentTemperature(); ok {
		re.temperatureSensor.CurrentTemperature.SetValue(t)
	}

	if re.humiditySensor != nil {
		if h, ok := re.currentRelativeHumidity(); ok {
			re.humiditySensor.CurrentRelativeHumidity.SetValue(h)
		}
	}

	if re.lightSensor != nil {
		if l, ok := re.currentAmbientLightLevel(); ok {
			re.lightSensor.CurrentAmbientLightLevel.SetValue(l)
		}
	}
}

func (re *Remo) currentTemperature() (float64, bool) {
	if e, ok := re.device.NewestEvents[natureremo.SensorTypeTemperature]; ok {
		t := e.Value

		if t < 0.0 || 100.0 < t {
			return 0.0, false
		}

		return math.Round(t*10.0) / 10.0, true
	}

	return 0.0, false
}

func (re *Remo) currentRelativeHumidity() (float64, bool) {
	if e, ok := re.device.NewestEvents[natureremo.SensorTypeHumidity]; ok {
		h := e.Value

		if h < 0.0 || 100.0 < h {
			return 0.0, false
		}

		return math.Round(h), true
	}

	return 0.0, false
}

func (re *Remo) currentAmbientLightLevel() (float64, bool) {
	if e, ok := re.device.NewestEvents[natureremo.SensortypeIllumination]; ok {
		l := e.Value

		if l < 0.0001 || 100000.0 < l {
			return 0.0, false
		}

		return l, true
	}

	return 0.0, false
}

type AccessoryUpdater interface {
	Update(d *natureremo.Device, a *natureremo.Appliance)
}

type AirCon struct {
	*accessory.Accessory
	thermostat *service.Thermostat
	client     *natureremo.Client
	context    context.Context
	device     *natureremo.Device
	appliance  *natureremo.Appliance
}

func NewAirCon(aid uint64, client *natureremo.Client, ctx context.Context, d *natureremo.Device, a *natureremo.Appliance) *AirCon {
	ai := AirCon{}
	ai.Accessory = accessory.New(
		accessory.Info{
			Name:         a.Nickname,
			Manufacturer: a.Model.Manufacturer,
			Model:        a.Model.Name,
			ID:           aid,
		},
		accessory.TypeAirConditioner,
	)

	ai.client = client
	ai.context = ctx
	ai.device = d
	ai.appliance = a

	ai.thermostat = service.NewThermostat()
	if s, ok := ai.currentHeatingCoolingState(); ok {
		ai.thermostat.CurrentHeatingCoolingState.SetValue(s)
	}

	if s, ok := ai.targetHeatingCoolingState(); ok {
		ai.thermostat.TargetHeatingCoolingState.SetValue(s)
	}
	ai.thermostat.TargetHeatingCoolingState.SetMaxValue(ai.targetHeatingCoolingStateMax())
	ai.thermostat.TargetHeatingCoolingState.OnValueRemoteUpdate(ai.updateTargetHeatingCoolingState)

	if t, ok := ai.currentTemperature(); ok {
		ai.thermostat.CurrentTemperature.SetValue(t)
	}

	if t, ok := ai.targetTemperature(); ok {
		ai.thermostat.TargetTemperature.SetValue(t)
	}
	min, max, step := ai.targetTemperatureProps()
	ai.thermostat.TargetTemperature.SetMinValue(min)
	ai.thermostat.TargetTemperature.SetMaxValue(max)
	ai.thermostat.TargetTemperature.SetStepValue(step)
	ai.thermostat.TargetTemperature.OnValueRemoteUpdate(ai.updateTargetTemperature)

	if unit, ok := ai.temperatureDisplayUnits(); ok {
		ai.thermostat.TemperatureDisplayUnits.SetValue(unit)
	}
	ai.thermostat.TemperatureDisplayUnits.OnValueRemoteUpdate(ai.updateTemperatureDisplayUnits)
	ai.AddService(ai.thermostat.Service)

	return &ai
}

func (ai *AirCon) Update(d *natureremo.Device, a *natureremo.Appliance) {
	ai.device = d
	ai.appliance = a

	if s, ok := ai.currentHeatingCoolingState(); ok {
		ai.thermostat.CurrentHeatingCoolingState.SetValue(s)
	}
	if s, ok := ai.targetHeatingCoolingState(); ok {
		ai.thermostat.TargetHeatingCoolingState.SetValue(s)
	}
	if t, ok := ai.currentTemperature(); ok {
		ai.thermostat.CurrentTemperature.SetValue(t)
	}
	if t, ok := ai.targetTemperature(); ok {
		ai.thermostat.TargetTemperature.SetValue(t)
	}
}

func (ai *AirCon) updateTargetHeatingCoolingState(s int) {
	if mode, button, ok := ai.operationModeAndButton(s); ok {
		ctx, cancel := context.WithTimeout(ai.context, timeout)
		defer cancel()

		settings := natureremo.AirConSettings{
			OperationMode: mode,
			Button:        button,
		}
		err := ai.client.ApplianceService.UpdateAirConSettings(ctx, ai.appliance, &settings)
		if err != nil {
			log.Print(err)
		}
	}
}

func (ai *AirCon) updateTargetTemperature(t float64) {
	if t, ok := ai.temperature(t); ok {
		ctx, cancel := context.WithTimeout(ai.context, timeout)
		defer cancel()

		settings := natureremo.AirConSettings{
			Temperature: t,
		}
		err := ai.client.ApplianceService.UpdateAirConSettings(ctx, ai.appliance, &settings)
		if err != nil {
			log.Print(err)
		}
	}
}

func (ai *AirCon) updateTemperatureDisplayUnits(units int) {
	// There is no way to change the display unit of the air conditioner.
}

func (ai *AirCon) currentHeatingCoolingState() (int, bool) {
	// Since there is no way to get the current mode of the air conditioner itself,
	// the last operation of the remote control is regarded as the current mode.
	switch ai.appliance.AirConSettings.Button {
	case natureremo.ButtonPowerOn:
		switch ai.appliance.AirConSettings.OperationMode {
		case natureremo.OperationModeAuto:
			return 0, false // Auto mode is not supported.
		case natureremo.OperationModeCool:
			return 2, true
		case natureremo.OperationModeWarm:
			return 1, true
		case natureremo.OperationModeDry:
			return 0, false // Dry mode is not supported.
		case natureremo.OperationModeBlow:
			return 0, false // Blow mode is not supported.
		}
	case natureremo.ButtonPowerOff:
		return 0, true
	}

	return 0, false
}

func (ai *AirCon) targetHeatingCoolingState() (int, bool) {
	switch ai.appliance.AirConSettings.Button {
	case natureremo.ButtonPowerOn:
		switch ai.appliance.AirConSettings.OperationMode {
		case natureremo.OperationModeAuto:
			return 0, false // Auto mode is not supported.
		case natureremo.OperationModeCool:
			return 2, true
		case natureremo.OperationModeWarm:
			return 1, true
		case natureremo.OperationModeDry:
			return 0, false // Dry mode is not supported.
		case natureremo.OperationModeBlow:
			return 0, false // Blow mode is not supported.
		}
	case natureremo.ButtonPowerOff:
		return 0, true
	}
	return 0, false
}

func (ai *AirCon) targetHeatingCoolingStateMax() int {
	// Disable the automatic mode of the air conditioner
	// because it requires the temperature to be specified as a relative value.
	return 2
}

func (ai *AirCon) currentTemperature() (float64, bool) {
	// There is no way to get the thermometer value of the air conditioner itself,
	// so the temperature in NatureRemo is considered to be the temperature of the air conditioner.
	if e, ok := ai.device.NewestEvents[natureremo.SensorTypeTemperature]; ok {
		t := e.Value

		if t < 0.0 || 100.0 < t {
			return 0.0, false
		}
		return math.Round(t*10.0) / 10.0, true
	}

	return 0.0, false
}

func (ai *AirCon) targetTemperature() (float64, bool) {
	t, err := strconv.ParseFloat(ai.appliance.AirConSettings.Temperature, 64)
	if err != nil {
		return 0.0, false
	}

	if t < 10.0 || 38.0 < t {
		return 0.0, false
	}

	return math.Round(t*10.0) / 10.0, true
}

func (ai *AirCon) targetTemperatureProps() (float64, float64, float64) {
	min := 100.0
	max := 0.0
	step := 1.0

	for _, mode := range []natureremo.OperationMode{natureremo.OperationModeCool, natureremo.OperationModeWarm} {
		if m, ok := ai.appliance.AirCon.Range.Modes[mode]; ok {
			prev := 0.0
			for i, value := range m.Temperature {
				curr, err := strconv.ParseFloat(value, 64)
				if err != nil {
					continue
				}

				min = math.Min(curr, min)
				max = math.Max(curr, max)

				if i > 0 {
					if diff := curr - prev; diff < step {
						step = diff
					}
				}
				prev = curr
			}
		}
	}

	min = math.Max(10.0, min)
	max = math.Min(38.0, max)

	// TODO: 下限を10.0よりも大きく設定するとiOSやmacOSのHome.appがフリーズする。
	min = 10.0

	return min, max, step
}

func (ai *AirCon) temperatureDisplayUnits() (int, bool) {
	switch ai.appliance.AirCon.TemperatureUnit {
	case natureremo.TemperatureUnitAuto:
		return 0, false // If the temperature unit is automatic, it is not supported.
	case natureremo.TemperatureUnitFahrenheit:
		return 1, true
	case natureremo.TemperatureUnitCelsius:
		return 0, true
	}

	return 0, false
}

func (ai *AirCon) operationModeAndButton(s int) (natureremo.OperationMode, natureremo.Button, bool) {
	switch s {
	case 0:
		return "", natureremo.ButtonPowerOff, true
	case 1:
		if _, ok := ai.appliance.AirCon.Range.Modes[natureremo.OperationModeWarm]; ok {
			return natureremo.OperationModeWarm, natureremo.ButtonPowerOn, true
		}
	case 2:
		if _, ok := ai.appliance.AirCon.Range.Modes[natureremo.OperationModeCool]; ok {
			return natureremo.OperationModeCool, natureremo.ButtonPowerOn, true
		}
	}

	return "", "", false
}

func (ai *AirCon) temperature(t float64) (string, bool) {
	var mode natureremo.OperationMode

	switch ai.thermostat.TargetHeatingCoolingState.GetValue() {
	case 0:
		return "", false
	case 1:
		mode = natureremo.OperationModeWarm
	case 2:
		mode = natureremo.OperationModeCool
	default:
		return "", false
	}

	if m, ok := ai.appliance.AirCon.Range.Modes[mode]; ok {
		for _, value := range m.Temperature {
			curr, err := strconv.ParseFloat(value, 64)
			if err != nil {
				continue
			}

			if curr == t {
				return value, true
			}
		}
	}

	return "", false
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

func (lig *lightAppliance) Update(dev *natureremo.Device, ali *natureremo.Appliance) {
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
	devices    map[string]RemoUpdater
	appliances map[string]AccessoryUpdater
	aids       map[string]uint64
}

func newApplication(ctx context.Context) *application {
	app := application{}
	app.client = natureremo.NewClient(os.Getenv("ACCESS_TOKEN"))
	app.context = ctx
	app.devices = make(map[string]RemoUpdater)
	app.appliances = make(map[string]AccessoryUpdater)
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

	app.devices = make(map[string]RemoUpdater, len(devs))
	app.appliances = make(map[string]AccessoryUpdater, len(alis))

	bri := NewBridge(app.getAid(app.user.ID), app.user)
	accs := make([]*accessory.Accessory, 0, len(app.devices)+len(app.appliances))
	devm := make(map[string]*natureremo.Device, len(devs))

	for _, dev := range devs {
		re := NewRemo(app.getAid(dev.ID), app.client, app.context, dev)
		app.devices[dev.ID] = re
		accs = append(accs, re.Accessory)
		devm[dev.ID] = dev
	}

	for _, ali := range alis {
		switch ali.Type {
		case natureremo.ApplianceTypeAirCon:
			ai := NewAirCon(app.getAid(ali.ID), app.client, app.context, devm[ali.Device.ID], ali)
			app.appliances[ali.ID] = ai
			accs = append(accs, ai.Accessory)
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
		app.devices[dev.ID].Update(dev)
		devm[dev.ID] = dev
	}

	for _, ali := range alis {
		if aupd, ok := app.appliances[ali.ID]; ok {
			aupd.Update(devm[ali.Device.ID], ali)
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
		if os.IsNotExist(err) {
			return nil
		}

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
