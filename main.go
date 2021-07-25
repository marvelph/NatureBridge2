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

type Remo struct {
	*accessory.Accessory
	temperatureSensor *service.TemperatureSensor
	humiditySensor    *service.HumiditySensor
	lightSensor       *service.LightSensor
	device            *natureremo.Device
}

func NewRemo(aid uint64, d *natureremo.Device) *Remo {
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

	if _, ok := re.device.NewestEvents[natureremo.SensorTypeIllumination]; ok {
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
	if e, ok := re.device.NewestEvents[natureremo.SensorTypeIllumination]; ok {
		l := e.Value

		if l < 0.0001 || 100000.0 < l {
			return 0.0, false
		}

		return l, true
	}

	return 0.0, false
}

type Updater interface {
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

	// TODO: 下限を10.0よりも大きく設定するとiOSやmacOSのHome.appがフリーズする。
	// min = math.Max(10.0, min)
	min = 10.0
	max = math.Min(38.0, max)

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

type Light struct {
	*accessory.Accessory
	lightbulb *service.Lightbulb
	client    *natureremo.Client
	context   context.Context
	device    *natureremo.Device
	appliance *natureremo.Appliance
}

func NewLight(aid uint64, client *natureremo.Client, ctx context.Context, d *natureremo.Device, a *natureremo.Appliance) *Light {
	li := Light{}
	li.Accessory = accessory.New(
		accessory.Info{
			Name:         a.Nickname,
			Manufacturer: a.Model.Manufacturer,
			Model:        a.Model.Name,
			ID:           aid,
		},
		accessory.TypeLightbulb,
	)

	li.client = client
	li.context = ctx
	li.device = d
	li.appliance = a

	li.lightbulb = service.NewLightbulb()
	if o, ok := li.on(); ok {
		li.lightbulb.On.SetValue(o)
	}
	li.lightbulb.On.OnValueRemoteUpdate(li.updateOn)
	li.AddService(li.lightbulb.Service)

	return &li
}

func (li *Light) Update(d *natureremo.Device, a *natureremo.Appliance) {
	li.device = d
	li.appliance = a

	if o, ok := li.on(); ok {
		li.lightbulb.On.SetValue(o)
	}
}

func (li *Light) updateOn(o bool) {
	ctx, cancel := context.WithTimeout(li.context, timeout)
	defer cancel()

	_, err := li.client.ApplianceService.SendLightSignal(ctx, li.appliance, li.power(o))
	if err != nil {
		log.Print(err)
	}
}

func (li *Light) on() (bool, bool) {
	switch li.appliance.Light.State.Power {
	case "on":
		return true, true
	case "off":
		return false, true
	}

	return false, false
}

func (li *Light) power(o bool) string {
	if o {
		return "on"
	} else {
		return "off"
	}
}

type Application struct {
	client      *natureremo.Client
	context     context.Context
	transport   hc.Transport
	user        *natureremo.User
	remos       map[string]*Remo
	accessories map[string]Updater
	aids        map[string]uint64
}

func NewApplication(ctx context.Context) *Application {
	app := Application{}
	app.client = natureremo.NewClient(os.Getenv("ACCESS_TOKEN"))
	app.context = ctx
	app.remos = map[string]*Remo{}
	app.accessories = map[string]Updater{}
	app.aids = map[string]uint64{}
	return &app
}

func (app *Application) ShutdownAndWait() {
	if app.transport != nil {
		<-app.transport.Stop()
		app.transport = nil
	}
}

func (app *Application) Update() error {
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
		app.update(ds, as)
	}

	return nil
}

func (app *Application) getAll() ([]*natureremo.Device, []*natureremo.Appliance, error) {
	if app.user == nil {
		ctx, cancel := context.WithTimeout(app.context, timeout)
		defer cancel()

		u, err := app.client.UserService.Me(ctx)
		if err != nil {
			return nil, nil, err
		}

		app.user = u
	}

	ctx, cancel := context.WithTimeout(app.context, timeout)
	defer cancel()

	ds, err := app.client.DeviceService.GetAll(ctx)
	if err != nil {
		return nil, nil, err
	}

	ctx, cancel = context.WithTimeout(app.context, timeout)
	defer cancel()

	as, err := app.client.ApplianceService.GetAll(ctx)
	if err != nil {
		return nil, nil, err
	}

	return ds, as, nil
}

func (app *Application) wasChanged(ds []*natureremo.Device, as []*natureremo.Appliance) bool {
	appliances := make([]*natureremo.Appliance, 0, len(as))

	for _, a := range as {
		switch a.Type {
		case natureremo.ApplianceTypeAirCon:
			appliances = append(appliances, a)
		case natureremo.ApplianceTypeLight:
			appliances = append(appliances, a)
		}
	}

	if len(app.remos) != len(ds) || len(app.accessories) != len(appliances) {
		return true
	}

	for _, d := range ds {
		if _, ok := app.remos[d.ID]; !ok {
			return true
		}
	}

	for _, a := range appliances {
		if _, ok := app.accessories[a.ID]; !ok {
			return true
		}
	}

	return false
}

func (app *Application) build(ds []*natureremo.Device, as []*natureremo.Appliance) error {
	err := app.loadAids()
	if err != nil {
		return err
	}

	if app.transport != nil {
		<-app.transport.Stop()
		app.transport = nil
	}

	app.remos = make(map[string]*Remo, len(ds))
	app.accessories = make(map[string]Updater, len(as))

	br := NewBridge(app.getAid(app.user.ID), app.user)

	devices := make(map[string]*natureremo.Device, len(ds))
	accessories := make([]*accessory.Accessory, 0, len(app.remos)+len(app.accessories))

	for _, d := range ds {
		devices[d.ID] = d

		re := NewRemo(app.getAid(d.ID), d)
		app.remos[d.ID] = re

		accessories = append(accessories, re.Accessory)
	}

	for _, a := range as {
		switch a.Type {
		case natureremo.ApplianceTypeAirCon:
			ai := NewAirCon(app.getAid(a.ID), app.client, app.context, devices[a.Device.ID], a)
			app.accessories[a.ID] = ai

			accessories = append(accessories, ai.Accessory)
		case natureremo.ApplianceTypeLight:
			li := NewLight(app.getAid(a.ID), app.client, app.context, devices[a.Device.ID], a)
			app.accessories[a.ID] = li

			accessories = append(accessories, li.Accessory)
		}
	}

	err = app.saveAids()
	if err != nil {
		return err
	}

	p := filepath.Join(os.Getenv("DATA_DIRECTORY"), "hc")
	err = os.MkdirAll(p, 0755)
	if err != nil {
		return err
	}

	config := hc.Config{
		StoragePath: p,
		Pin:         os.Getenv("PIN"),
	}
	transport, err := hc.NewIPTransport(config, br.Accessory, accessories...)
	if err != nil {
		return err
	}

	app.transport = transport

	go func() {
		app.transport.Start()
	}()

	return nil
}

func (app *Application) update(ds []*natureremo.Device, as []*natureremo.Appliance) {
	devices := make(map[string]*natureremo.Device, len(ds))

	for _, d := range ds {
		devices[d.ID] = d

		app.remos[d.ID].Update(d)
	}

	for _, a := range as {
		if ac, ok := app.accessories[a.ID]; ok {
			ac.Update(devices[a.Device.ID], a)
		}
	}
}

func (app *Application) getAid(id string) uint64 {
	if aid, ok := app.aids[id]; ok {
		return aid
	}

	aid := uint64(len(app.aids) + 1)
	app.aids[id] = aid

	return aid
}

func (app *Application) saveAids() error {
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

func (app *Application) loadAids() error {
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
	app := NewApplication(ctx)
	defer app.ShutdownAndWait()

	err := app.Update()
	if err != nil {
		log.Print(err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			err := app.Update()
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

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(c)

	go func() {
		defer cancel()

		<-c
	}()

	mainHandler(ctx)
}
