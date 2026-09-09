package amqp

import "strings"

// namePrefix marks every object that Citadel owns, as opposed to an object the
// application under test declares.
const namePrefix = "citadel."

// IsCitadelQueue reports whether a queue belongs to Citadel rather than to the
// application. The spy queues, the lock queue and the unroutable queue all
// carry the prefix, so a check over app-owned queues can exclude them.
func IsCitadelQueue(name string) bool {
	return strings.HasPrefix(name, namePrefix)
}

// SpyQueueName returns the spy queue name for the given exchange.
// The format is "citadel.spy.<exchange>" with dots in the exchange preserved.
func SpyQueueName(exchange string) string {
	return "citadel.spy." + exchange
}

// LockQueueName returns the name of the exclusive queue that serves as the environment lock.
func LockQueueName() string {
	return "citadel.lock"
}

// UnroutableQueueName returns the name of the queue that catches unroutable messages.
func UnroutableQueueName() string {
	return "citadel.unroutable"
}

// UnroutableExchangeName returns the alternate exchange name for unroutable messages.
func UnroutableExchangeName() string {
	return "citadel.unroutable.ae"
}
