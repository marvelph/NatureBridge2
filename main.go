package main

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"path/filepath"
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
	nr.Accessory = accessory.New(accessory.Info{Name: d.Name, Manufacturer: "Nature", FirmwareRevision: d.FirmwareVersion, ID: id}, accessory.TypeSensor)

	nr.temperatureSensor = service.NewTemperatureSensor()
	nr.temperatureSensor.CurrentTemperature.SetValue(d.NewestEvents[natureremo.SensorTypeTemperature].Value)
	nr.AddService(nr.temperatureSensor.Service)

	if v, ok := d.NewestEvents[natureremo.SensorTypeHumidity]; ok {
		nr.humiditySensor = service.NewHumiditySensor()
		nr.humiditySensor.CurrentRelativeHumidity.SetValue(v.Value)
		nr.AddService(nr.humiditySensor.Service)
	}

	if v, ok := d.NewestEvents[natureremo.SensortypeIllumination]; ok {
		nr.lightSensor = service.NewLightSensor()
		nr.lightSensor.CurrentAmbientLightLevel.SetValue(v.Value)
		nr.AddService(nr.lightSensor.Service)
	}

	nr.client = cli
	nr.context = ctx
	nr.device = d
	return &nr
}

func (nr *natureRemo) update(d *natureremo.Device) {
	nr.device = d

	nr.temperatureSensor.CurrentTemperature.SetValue(d.NewestEvents[natureremo.SensorTypeTemperature].Value)

	if v, ok := d.NewestEvents[natureremo.SensorTypeHumidity]; nr.humiditySensor != nil && ok {
		nr.humiditySensor.CurrentRelativeHumidity.SetValue(v.Value)
	}

	if v, ok := d.NewestEvents[natureremo.SensortypeIllumination]; nr.lightSensor != nil && ok {
		nr.lightSensor.CurrentAmbientLightLevel.SetValue(v.Value)
	}
}

type applianceUpdater interface {
	update(d *natureremo.Device, a *natureremo.Appliance)
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
	la.Accessory = accessory.New(accessory.Info{Name: a.Nickname, Manufacturer: a.Model.Manufacturer, Model: a.Model.Name, ID: id}, accessory.TypeLightbulb)
	la.client = cli
	la.context = ctx
	la.device = d
	la.appliance = a

	la.lightbulb = service.NewLightbulb()
	la.lightbulb.On.SetValue(la.toHomeKitOn(a.Light.State.Power))
	la.lightbulb.On.OnValueRemoteUpdate(la.onValueRemoteUpdate)
	la.AddService(la.lightbulb.Service)

	return &la
}

func (la *lightAppliance) update(d *natureremo.Device, a *natureremo.Appliance) {
	la.device = d
	la.appliance = a

	la.lightbulb.On.SetValue(la.toHomeKitOn(a.Light.State.Power))
}

func (la *lightAppliance) onValueRemoteUpdate(on bool) {
	ctx, cancel := context.WithTimeout(la.context, timeout)
	defer cancel()

	_, err := la.client.ApplianceService.SendLightSignal(ctx, la.appliance, la.toNatureOn(on))
	if err != nil {
		log.Print(err)
	}
}

func (la *lightAppliance) toHomeKitOn(v string) bool {
	return v == "on"
}

func (la *lightAppliance) toNatureOn(v bool) string {
	if v {
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
