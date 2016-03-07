package balancer

import (
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/getlantern/withtimeout"
)

// Dialer captures the configuration for dialing arbitrary addresses.
type Dialer struct {
	// Label: optional label with which to tag this dialer for debug logging.
	Label string

	// Weight: determines how often this Dialer is used relative to the other
	// Dialers on the balancer.
	Weight int

	// QOS: identifies the quality of service provided by this dialer. Higher
	// numbers equal higher quality. "Quality" in this case is loosely defined,
	// but can mean things such as reliability, speed, etc.
	QOS int

	// Dial: this function dials the given network, addr.
	Dial func(network, addr string) (net.Conn, error)

	// OnClose: (optional) callback for when this dialer is stopped.
	OnClose func()

	// Check: (optional) - When dialing fails, this Dialer is deactivated (taken
	// out of rotation). Check is a function that's used periodically after a
	// failed dial to check whether or not Dial works again. As soon as there is
	// a successful check, this Dialer will be activated (put back in rotation).
	//
	// If Check is not specified, a default Check will be used that makes an
	// HTTP request to http://www.google.com/humans.txt using this Dialer.
	//
	// Checks are scheduled at exponentially increasing intervals that are
	// capped at 1 minute.
	Check func() bool

	// Determines wheter a dialer can be trusted with unencrypted traffic.
	Trusted bool

	AuthToken string
}

var (
	maxCheckTimeout = 5 * time.Second
)

type dialer struct {
	*Dialer
	consecSuccesses uint32
	consecFailures  uint32
	closeCh         chan struct{}
	errCh           chan struct{}
}

func (d *dialer) start() {
	d.consecSuccesses = 1 // be optimistic
	// to avoid blocking sender, make it buffered
	d.closeCh = make(chan struct{}, 1)
	d.errCh = make(chan struct{}, 1)
	if d.Check == nil {
		d.Check = d.defaultCheck
	}

	longDuration := 1000000 * time.Hour
	go func() {
		timer := time.NewTimer(longDuration)
		for {
			cf := atomic.LoadUint32(&d.consecFailures)
			timeout := time.Duration(cf*cf) * 100 * time.Millisecond
			if timeout > maxCheckTimeout {
				timeout = maxCheckTimeout
			}
			if timeout == 0 {
				timeout = longDuration
			}
			timer.Reset(timeout)
			select {
			case <-d.closeCh:
				log.Tracef("Dialer %s stopped", d.Label)
				if d.OnClose != nil {
					d.OnClose()
				}
				return
			case <-d.errCh:
				atomic.StoreUint32(&d.consecSuccesses, 0)
				log.Tracef("Mark dialer %s as inactive, scheduling check", d.Label)
				atomic.AddUint32(&d.consecFailures, 1)
			case <-timer.C:
				ok := d.Check()
				if ok {
					atomic.StoreUint32(&d.consecFailures, 0)
				} else {
					d.errCh <- struct{}{}
				}
			}
		}
	}()
}

func (d *dialer) isActive() bool {
	return atomic.LoadUint32(&d.consecSuccesses) > 0
}

func (d *dialer) checkedDial(network, addr string) (net.Conn, error) {
	conn, err := d.Dial(network, addr)
	if err != nil {
		d.onError(err)
	} else {
		atomic.AddUint32(&d.consecSuccesses, 1)
	}
	return conn, err
}

func (d *dialer) onError(err error) {
	select {
	case d.errCh <- struct{}{}:
		log.Trace("Error reported")
	default:
		log.Trace("Errors already pending, ignoring new one")
	}
}

func (d *dialer) stop() {
	d.closeCh <- struct{}{}
}

func (d *dialer) defaultCheck() bool {
	client := &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives: true,
			Dial:              d.Dial,
		},
	}
	ok, timedOut, _ := withtimeout.Do(60*time.Second, func() (interface{}, error) {
		req, err := http.NewRequest("GET", "http://www.google.com/humans.txt", nil)
		if err != nil {
			log.Errorf("Could not create HTTP request?")
			return false, nil
		}
		req.Header.Set("X-LANTERN-AUTH-TOKEN", d.AuthToken)
		resp, err := client.Do(req)
		if err != nil {
			log.Debugf("Error testing dialer %s to humans.txt: %s", d.Label, err)
			return false, nil
		}
		if err := resp.Body.Close(); err != nil {
			log.Debugf("Unable to close response body: %v", err)
		}
		log.Tracef("Tested dialer %s to humans.txt, status code %d", d.Label, resp.StatusCode)
		return resp.StatusCode == 200, nil
	})
	if timedOut {
		log.Errorf("Timed out checking dialer at: %v", d.Label)
	}
	return !timedOut && ok.(bool)
}
