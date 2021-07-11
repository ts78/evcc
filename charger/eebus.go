// +build eebus

package charger

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/amp-x/eebus/app"
	"github.com/amp-x/eebus/communication"
	"github.com/amp-x/eebus/ship"
	"github.com/amp-x/eebus/spine"
	"github.com/andig/evcc/api"
	"github.com/andig/evcc/core"
	"github.com/andig/evcc/server"
	"github.com/andig/evcc/util"
)

type EEBus struct {
	log           *util.Logger
	cc            *communication.ConnectionController
	lp            core.LoadPointAPI
	forcePVLimits bool

	communicationStandard           communication.EVCommunicationStandardEnumType
	socSupportAvailable             bool
	selfConsumptionSupportAvailable bool

	maxCurrent          float64
	connected           bool
	expectedEnableState bool
}

func init() {
	registry.Add("eebus", NewEEBusFromConfig)
}

// NewEEBusFromConfig creates an EEBus charger from generic config
func NewEEBusFromConfig(other map[string]interface{}) (api.Charger, error) {
	cc := struct {
		Ski           string
		ForcePVLimits bool
	}{}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	return NewEEBus(cc.Ski, cc.ForcePVLimits)
}

// NewEEBus creates EEBus charger
func NewEEBus(ski string, forcePVLimits bool) (*EEBus, error) {
	log := util.NewLogger("eebus")

	if server.EEBusInstance == nil {
		return nil, errors.New("eebus not configured")
	}

	c := &EEBus{
		log:                   log,
		forcePVLimits:         forcePVLimits,
		communicationStandard: communication.EVCommunicationStandardEnumTypeUnknown,
	}

	server.EEBusInstance.Register(ski, c.onConnect, c.onDisconnect)

	return c, nil
}

var eebusDevice spine.Device
var once sync.Once

func (c *EEBus) onConnect(ski string, conn ship.Conn) error {
	once.Do(func() {
		eebusDevice = app.HEMS(server.EEBusInstance.DeviceInfo())
	})
	c.cc = communication.NewConnectionController(c.log.TRACE, conn, eebusDevice)
	c.cc.SetDataUpdateHandler(c.dataUpdateHandler)
	err := c.cc.Boot()
	c.connected = true
	c.expectedEnableState = false
	return err
}

func (c *EEBus) onDisconnect(ski string) {
	c.connected = false
}

func (c *EEBus) setLoadpointMinMaxLimits(data *communication.EVSEClientDataType) {
	if c.lp == nil {
		return
	}

	newMin := data.EVData.LimitsL1.Min
	newMax := data.EVData.LimitsL1.Max

	if c.lp.GetMinCurrent() != newMin && newMin > 0 {
		c.lp.SetMinCurrent(newMin)
	}
	if c.lp.GetMaxCurrent() != newMax && newMax > 0 {
		c.lp.SetMaxCurrent(newMax)
	}

	// TODO uncomment once the API is available
	// c.lp.SetPhases(int64(data.EVData.ConnectedPhases))
}

func (c *EEBus) showCurrentChargingSetup() {
	data, err := c.cc.GetData()
	if err != nil {
		return
	}

	prevComStandard := c.communicationStandard
	prevSoCSupport := c.socSupportAvailable
	prevSelfConsumptionSupport := c.selfConsumptionSupportAvailable

	if prevComStandard != data.EVData.CommunicationStandard {
		c.communicationStandard = data.EVData.CommunicationStandard
		timestamp := time.Now()
		c.log.WARN.Println(timestamp.Format("2006-01-02 15:04:05"), " ev-charger-communication changed from ", prevComStandard, " to ", data.EVData.CommunicationStandard)
	}

	if prevSoCSupport != data.EVData.UCSoCAvailable {
		c.socSupportAvailable = data.EVData.UCSoCAvailable
		timestamp := time.Now()
		c.log.WARN.Println(timestamp.Format("2006-01-02 15:04:05"), " ev-charger-soc support changed from ", prevSoCSupport, " to ", data.EVData.UCSoCAvailable)
	}

	if prevSelfConsumptionSupport != data.EVData.UCSelfConsumptionAvailable {
		c.selfConsumptionSupportAvailable = data.EVData.UCSelfConsumptionAvailable
		timestamp := time.Now()
		c.log.WARN.Println(timestamp.Format("2006-01-02 15:04:05"), " ev-charger-self-consumption-support support changed from ", prevSelfConsumptionSupport, " to ", data.EVData.UCSelfConsumptionAvailable)
	}

}

func (c *EEBus) dataUpdateHandler(dataType communication.EVDataElementUpdateType, data *communication.EVSEClientDataType) {
	// we receive data, so it is connected
	c.connected = true

	c.showCurrentChargingSetup()

	switch dataType {
	case communication.EVDataElementUpdateUseCaseSelfConsumption:
		// if availability of self consumption use case changes, resend the current charging limit
		c.writeCurrentLimitData([]float64{c.maxCurrent, c.maxCurrent, c.maxCurrent})
	// case communication.EVDataElementUpdateUseCaseSoC:
	case communication.EVDataElementUpdateEVConnectionState:
		if data.EVData.ChargeState == communication.EVChargeStateEnumTypeUnplugged {
			c.expectedEnableState = false
		}
		c.setLoadpointMinMaxLimits(data)
	case communication.EVDataElementUpdateCommunicationStandard:
		c.communicationStandard = data.EVData.CommunicationStandard
		c.setLoadpointMinMaxLimits(data)
	case communication.EVDataElementUpdateAsymetricChargingType:
		c.setLoadpointMinMaxLimits(data)
	// case communication.EVDataElementUpdateEVSEOperationState:
	// case communication.EVDataElementUpdateEVChargeState:
	case communication.EVDataElementUpdateConnectedPhases:
		c.setLoadpointMinMaxLimits(data)
	case communication.EVDataElementUpdatePowerLimits:
		c.setLoadpointMinMaxLimits(data)
	case communication.EVDataElementUpdateAmperageLimits:
		c.setLoadpointMinMaxLimits(data)
	}
}

// Status implements the api.Charger interface
func (c *EEBus) Status() (api.ChargeStatus, error) {
	data, err := c.cc.GetData()
	if err != nil {
		c.log.TRACE.Printf("status: no eebus data available yet")
		return api.StatusNone, err
	}

	currentState := data.EVData.ChargeState

	if !c.connected {
		c.log.TRACE.Printf("status: charger reported as disconnected")
		return api.StatusNone, fmt.Errorf("charger reported as disconnected")
	}

	switch currentState {
	case communication.EVChargeStateEnumTypeUnknown:
		return api.StatusA, nil
	case communication.EVChargeStateEnumTypeUnplugged: // Unplugged
		return api.StatusA, nil
	case communication.EVChargeStateEnumTypeFinished, communication.EVChargeStateEnumTypePaused: // Finished, Paused
		return api.StatusB, nil
	case communication.EVChargeStateEnumTypeError: // Error
		return api.StatusF, nil
	case communication.EVChargeStateEnumTypeActive: // Active
		if data.EVData.Measurements.PowerL1 > 50 || data.EVData.Measurements.PowerL2 > 50 || data.EVData.Measurements.PowerL3 > 50 {
			return api.StatusC, nil
		}
		return api.StatusB, nil
	}
	return api.StatusNone, fmt.Errorf("properties unknown result: %s", currentState)
}

// Enabled implements the api.Charger interface
func (c *EEBus) Enabled() (bool, error) {
	// we might already be enabled and charging due to connection issues
	data, err := c.cc.GetData()
	if err == nil {
		chargeState, _ := c.Status()
		if chargeState == api.StatusB || chargeState == api.StatusC {
			// we assume that if any current power value of any phase is >50W, then charging is active and enabled is true
			if data.EVData.Measurements.PowerL1 > 50 || data.EVData.Measurements.PowerL2 > 50 || data.EVData.Measurements.PowerL3 > 50 {
				if c.expectedEnableState == false {
					c.expectedEnableState = true
				}
			}
		}
		// handle ev being disconnected
		if data.EVData.ChargeState == communication.EVChargeStateEnumTypeUnplugged ||
			data.EVData.ChargeState == communication.EVChargeStateEnumTypeUnknown {
			c.expectedEnableState = false
		}
	}

	// return the save enable state as we assume enabling/disabling always works
	return c.expectedEnableState, nil
}

// Enable implements the api.Charger interface
func (c *EEBus) Enable(enable bool) error {
	data, err := c.cc.GetData()
	if err != nil {
		c.log.TRACE.Printf("enable: no eebus data available yet")
		return err
	}

	if data.EVData.ChargeState == communication.EVChargeStateEnumTypeUnplugged {
		c.log.TRACE.Printf("currents: ev reported as unplugged")
		return errors.New("can not enable/disable charging as ev is unplugged")
	}

	c.expectedEnableState = enable

	if !enable {
		// Important notes on enabling/disabling!!
		// ISO15118 mode:
		//   non-asymmetric or all phases set to 0: the OBC will wait for 1 minute, if the values remain after 1 min, it will pause then
		//   asymmetric and only some phases set to 0: no pauses or waiting for changes required
		//   asymmetric mode requires Plug & Charge (PnC) and Value Added Services (VAS)
		// IEC61851 mode:
		//   switching between 1/3 phases: stop charging, pause for 2 minutes, change phases, resume charging
		//   frequent switching should be avoided by all means!
		c.maxCurrent = 0
		return c.writeCurrentLimitData([]float64{0.0, 0.0, 0.0})
	}

	// if we set MaxCurrent > Min value and then try to enable the charger, it would reset it to min
	if c.maxCurrent > 0 {
		return c.writeCurrentLimitData([]float64{c.maxCurrent, c.maxCurrent, c.maxCurrent})
	}

	return c.writeCurrentLimitData([]float64{data.EVData.LimitsL1.Min, data.EVData.LimitsL2.Min, data.EVData.LimitsL3.Min})
}

// returns true if the connected EV supports charging recommandation
func (c *EEBus) optimizationSelfConsumptionAvailable() bool {
	data, err := c.cc.GetData()
	if err == nil {
		return data.EVData.UCSelfConsumptionAvailable
	}

	return false
}

func (c *EEBus) writeCurrentLimitData(currents []float64) error {
	data, err := c.cc.GetData()
	if err != nil {
		return err
	}

	selfConsumptionCurrents := []float64{0.0, 0.0, 0.0}
	overloadProtectionCurrents := currents

	// are the limits obligations or recommendations
	// in the scenarios IEC, ISO without asymetric charging, the limits are always obligations
	obligationEnabled := true

	if c.optimizationSelfConsumptionAvailable() {
		obligationEnabled = c.forcePVLimits
		if c.lp != nil && !obligationEnabled {
			// recommendations only work in PV modes
			chargeMode := c.lp.GetMode()
			if chargeMode != api.ModePV && chargeMode != api.ModeMinPV {
				obligationEnabled = true
			}
		}
	}

	// when recommending a current make sure the overload protection limit is set to max
	if !obligationEnabled {
		selfConsumptionCurrents = currents
		overloadProtectionCurrents = []float64{data.EVData.LimitsL1.Max, data.EVData.LimitsL2.Max, data.EVData.LimitsL3.Max}
	}

	return c.cc.WriteCurrentLimitData(overloadProtectionCurrents, selfConsumptionCurrents, data.EVData)
}

// MaxCurrent implements the api.Charger interface
func (c *EEBus) MaxCurrent(current int64) error {
	return c.MaxCurrentMillis(float64(current))
}

var _ api.ChargerEx = (*EEBus)(nil)

// MaxCurrentMillis implements the api.ChargerEx interface
func (c *EEBus) MaxCurrentMillis(current float64) error {
	data, err := c.cc.GetData()
	if err != nil {
		c.log.TRACE.Printf("currents: no eebus data available yet")
		return err
	}

	if data.EVData.ChargeState == communication.EVChargeStateEnumTypeUnplugged {
		c.log.TRACE.Printf("currents: ev reported as unplugged")
		return errors.New("can't set new current as ev is unplugged")
	}

	if data.EVData.LimitsL1.Min == 0 {
		c.log.TRACE.Println("we did not yet receive min and max currents to validate the call of MaxCurrent, use it as is")
	}

	if current < data.EVData.LimitsL1.Min {
		c.log.TRACE.Printf("current value %f is lower than the allowed minimum value %f", current, data.EVData.LimitsL1.Min)
		current = data.EVData.LimitsL1.Min
	}

	if current > data.EVData.LimitsL1.Max {
		c.log.TRACE.Printf("current value %f is higher than the allowed maximum value %f", current, data.EVData.LimitsL1.Max)
		current = data.EVData.LimitsL1.Max
	}

	c.maxCurrent = current

	// TODO error handling

	c.log.TRACE.Printf("currents: returning %f", current)

	currents := []float64{current, current, current}
	return c.writeCurrentLimitData(currents)
}

var _ api.Meter = (*EEBus)(nil)

// CurrentPower implements the api.Meter interface
func (c *EEBus) CurrentPower() (float64, error) {
	data, err := c.cc.GetData()
	if err != nil {
		c.log.TRACE.Printf("current power: no eebus data available yet")
		return 0, err
	}

	if data.EVData.ChargeState == communication.EVChargeStateEnumTypeUnplugged {
		c.log.TRACE.Printf("current power: ev reported as unplugged")
		return 0, nil
	}

	power := data.EVData.Measurements.PowerL1 + data.EVData.Measurements.PowerL2 + data.EVData.Measurements.PowerL3
	c.log.TRACE.Printf("current power: returning %f", power)

	return power, nil
}

var _ api.ChargeRater = (*EEBus)(nil)

// ChargedEnergy implements the api.ChargeRater interface
func (c *EEBus) ChargedEnergy() (float64, error) {
	data, err := c.cc.GetData()
	if err != nil {
		c.log.TRACE.Printf("charged energy: no eebus data available yet")
		return 0, err
	}

	if data.EVData.ChargeState == communication.EVChargeStateEnumTypeUnplugged {
		c.log.TRACE.Printf("charged energy: ev reported as unplugged")
		return 0, nil
	}

	energy := data.EVData.Measurements.ChargedEnergy / 1000
	c.log.TRACE.Printf("charged energy: returning %f", energy)

	return energy, nil
}

// var _ api.ChargeTimer = (*EEBus)(nil)

// // ChargingTime implements the api.ChargeTimer interface
// func (c *EEBus) ChargingTime() (time.Duration, error) {
// 	// var currentSession MCCCurrentSession
// 	// if err := mcc.getEscapedJSON(mcc.apiURL(mccAPICurrentSession), &currentSession); err != nil {
// 	// 	return 0, err
// 	// }

// 	// return time.Duration(currentSession.Duration * time.Second), nil
// 	return 0, nil
// }

var _ api.MeterCurrent = (*EEBus)(nil)

// Currents implements the api.MeterCurrent interface
func (c *EEBus) Currents() (float64, float64, float64, error) {
	data, err := c.cc.GetData()
	if err != nil {
		c.log.TRACE.Printf("currents: no eebus data available yet")
		return 0, 0, 0, err
	}

	if data.EVData.ChargeState == communication.EVChargeStateEnumTypeUnplugged {
		c.log.TRACE.Printf("currents: ev reported as unplugged")
		return 0, 0, 0, nil
	}

	c.log.TRACE.Printf("currents: returning %f, %f, %f, ", data.EVData.Measurements.CurrentL1, data.EVData.Measurements.CurrentL2, data.EVData.Measurements.CurrentL3)

	return data.EVData.Measurements.CurrentL1, data.EVData.Measurements.CurrentL2, data.EVData.Measurements.CurrentL3, nil
}

var _ api.Identifier = (*EEBus)(nil)

// Identifier implements the api.Identifier interface
func (c *EEBus) Identify() (string, error) {
	data, err := c.cc.GetData()
	if err != nil {
		c.log.TRACE.Printf("identify: no eebus data available yet")
		return "", err
	}

	if !c.connected {
		c.log.TRACE.Printf("identify: charger reported as disconnected")
		return "", nil
	}

	if data.EVData.ChargeState == communication.EVChargeStateEnumTypeUnplugged || data.EVData.ChargeState == communication.EVChargeStateEnumTypeUnknown {
		c.log.TRACE.Printf("identify: ev reported as unplugged or unknown")
		return "", nil
	}

	if len(data.EVData.Identification) > 0 {
		c.log.TRACE.Printf("identify: returning %s", data.EVData.Identification)
		return data.EVData.Identification, nil
	}

	c.log.TRACE.Printf("identify: returning nothing")
	return "", api.ErrMustRetry
}

var _ api.Battery = (*EEBus)(nil)

// SoC implements the api.Vehicle interface
func (c *EEBus) SoC() (float64, error) {
	data, err := c.cc.GetData()
	if err != nil {
		c.log.TRACE.Printf("soc: no eebus data available yet")
		return 0, api.ErrNotAvailable
	}

	if !data.EVData.UCSoCAvailable || !data.EVData.SoCDataAvailable {
		c.log.TRACE.Printf("soc: feature not available")
		return 0, api.ErrNotAvailable
	}

	c.log.TRACE.Printf("soc: returning %f", data.EVData.Measurements.SoC)
	return data.EVData.Measurements.SoC, nil
}

var _ core.LoadpointController = (*EEBus)(nil)

// LoadpointControl implements core.LoadpointController
func (c *EEBus) LoadpointControl(lp core.LoadPointAPI) {
	c.lp = lp

	// set current known min, max current limits
	data, err := c.cc.GetData()
	if err != nil {
		return
	}
	c.setLoadpointMinMaxLimits(data)
	c.showCurrentChargingSetup()
}
