from collections import defaultdict
from skidl import Pin, Part, Alias, SchLib, SKIDL, TEMPLATE

from skidl.pin import pin_types

SKIDL_lib_version = '0.0.1'

totem = SchLib(tool=SKIDL).add_parts(*[
        Part(**{ 'name':'ESP32-WROOM-32E', 'dest':TEMPLATE, 'tool':SKIDL, 'aliases':Alias({'ESP32-WROOM-32E'}), 'ref_prefix':'U', 'fplist':None, 'footprint':None, 'keywords':None, 'description':'', 'datasheet':None, 'pins':[
            Pin(num='3V3',name='3V3',func=pin_types.PWRIN),
            Pin(num='GND',name='GND',func=pin_types.PWRIN),
            Pin(num='0',name='IO0_BTN_SOS',func=pin_types.INPUT),
            Pin(num='4',name='IO4_BTN_PWR_LATCH',func=pin_types.BIDIR),
            Pin(num='14',name='IO14_SOS_LED',func=pin_types.OUTPUT),
            Pin(num='18',name='IO18_RING_DATA',func=pin_types.OUTPUT),
            Pin(num='19',name='IO19_RING_EN',func=pin_types.OUTPUT),
            Pin(num='21',name='IO21_CRYSTAL_DATA',func=pin_types.OUTPUT),
            Pin(num='25',name='IO25_I2C0_SDA',func=pin_types.BIDIR),
            Pin(num='26',name='IO26_I2C0_SCL',func=pin_types.OUTPUT),
            Pin(num='27',name='IO27_TOUCH',func=pin_types.BIDIR),
            Pin(num='34',name='IO34_VBAT_SENSE',func=pin_types.INPUT),
            Pin(num='36',name='IO36_MIC',func=pin_types.INPUT),
            Pin(num='39',name='IO39_VBUS_SENSE',func=pin_types.INPUT),
            Pin(num='17',name='IO17_GNSS_TX',func=pin_types.OUTPUT),
            Pin(num='16',name='IO16_GNSS_RX',func=pin_types.INPUT)] }),
        Part(**{ 'name':'MAX-M10S', 'dest':TEMPLATE, 'tool':SKIDL, 'aliases':Alias({'MAX-M10S'}), 'ref_prefix':'U', 'fplist':None, 'footprint':None, 'keywords':None, 'description':'', 'datasheet':None, 'pins':[
            Pin(num='VCC',name='VCC',func=pin_types.PWRIN),
            Pin(num='GND',name='GND',func=pin_types.PWRIN),
            Pin(num='RXD',name='RXD',func=pin_types.INPUT),
            Pin(num='TXD',name='TXD',func=pin_types.OUTPUT),
            Pin(num='RF',name='RF_IN',func=pin_types.PASSIVE)] }),
        Part(**{ 'name':'TP4056', 'dest':TEMPLATE, 'tool':SKIDL, 'aliases':Alias({'TP4056'}), 'ref_prefix':'U', 'fplist':None, 'footprint':None, 'keywords':None, 'description':'', 'datasheet':None, 'pins':[
            Pin(num='1',name='TEMP',func=pin_types.INPUT),
            Pin(num='2',name='PROG',func=pin_types.PASSIVE),
            Pin(num='3',name='GND',func=pin_types.PWRIN),
            Pin(num='4',name='VCC',func=pin_types.PWRIN),
            Pin(num='5',name='BAT',func=pin_types.PWROUT),
            Pin(num='6',name='BAT',func=pin_types.PASSIVE),
            Pin(num='7',name='CHRG',func=pin_types.OPENCOLL),
            Pin(num='8',name='STDBY',func=pin_types.OPENCOLL)] }),
        Part(**{ 'name':'LDO_3V3', 'dest':TEMPLATE, 'tool':SKIDL, 'aliases':Alias({'LDO_3V3'}), 'ref_prefix':'U', 'fplist':None, 'footprint':None, 'keywords':None, 'description':'', 'datasheet':None, 'pins':[
            Pin(num='1',name='IN',func=pin_types.PWRIN),
            Pin(num='2',name='GND',func=pin_types.PWRIN),
            Pin(num='3',name='EN',func=pin_types.INPUT),
            Pin(num='4',name='NC',func=pin_types.NOCONNECT),
            Pin(num='5',name='OUT',func=pin_types.PWROUT)] }),
        Part(**{ 'name':'ICM-20948', 'dest':TEMPLATE, 'tool':SKIDL, 'aliases':Alias({'ICM-20948'}), 'ref_prefix':'U', 'fplist':None, 'footprint':None, 'keywords':None, 'description':'', 'datasheet':None, 'pins':[
            Pin(num='VDD',name='VDD',func=pin_types.PWRIN),
            Pin(num='GND',name='GND',func=pin_types.PWRIN),
            Pin(num='SDA',name='SDA',func=pin_types.BIDIR),
            Pin(num='SCL',name='SCL',func=pin_types.INPUT)] }),
        Part(**{ 'name':'HALO_RING_60PX', 'dest':TEMPLATE, 'tool':SKIDL, 'aliases':Alias({'HALO_RING_60PX'}), 'ref_prefix':'DS', 'fplist':None, 'footprint':None, 'keywords':None, 'description':'', 'datasheet':None, 'pins':[
            Pin(num='VDD',name='VDD',func=pin_types.PWRIN),
            Pin(num='GND',name='GND',func=pin_types.PWRIN),
            Pin(num='DIN',name='DIN',func=pin_types.INPUT)] }),
        Part(**{ 'name':'CRYSTAL_7PX', 'dest':TEMPLATE, 'tool':SKIDL, 'aliases':Alias({'CRYSTAL_7PX'}), 'ref_prefix':'DS', 'fplist':None, 'footprint':None, 'keywords':None, 'description':'', 'datasheet':None, 'pins':[
            Pin(num='VDD',name='VDD',func=pin_types.PWRIN),
            Pin(num='GND',name='GND',func=pin_types.PWRIN),
            Pin(num='DIN',name='DIN',func=pin_types.INPUT)] }),
        Part(**{ 'name':'LED_SOS', 'dest':TEMPLATE, 'tool':SKIDL, 'aliases':Alias({'LED_SOS'}), 'ref_prefix':'DS', 'fplist':None, 'footprint':None, 'keywords':None, 'description':'', 'datasheet':None, 'pins':[
            Pin(num='A',name='A',func=pin_types.PASSIVE),
            Pin(num='K',name='K',func=pin_types.PASSIVE)] }),
        Part(**{ 'name':'PMOS_LED_SW', 'dest':TEMPLATE, 'tool':SKIDL, 'aliases':Alias({'PMOS_LED_SW'}), 'ref_prefix':'Q', 'fplist':None, 'footprint':None, 'keywords':None, 'description':'', 'datasheet':None, 'pins':[
            Pin(num='G',name='G',func=pin_types.INPUT),
            Pin(num='S',name='S',func=pin_types.PASSIVE),
            Pin(num='D',name='D',func=pin_types.PASSIVE)] }),
        Part(**{ 'name':'USB-C', 'dest':TEMPLATE, 'tool':SKIDL, 'aliases':Alias({'USB-C'}), 'ref_prefix':'J', 'fplist':None, 'footprint':None, 'keywords':None, 'description':'', 'datasheet':None, 'pins':[
            Pin(num='VBUS',name='VBUS',func=pin_types.PWROUT),
            Pin(num='GND',name='GND',func=pin_types.PWROUT)] }),
        Part(**{ 'name':'JST_BATT', 'dest':TEMPLATE, 'tool':SKIDL, 'aliases':Alias({'JST_BATT'}), 'ref_prefix':'J', 'fplist':None, 'footprint':None, 'keywords':None, 'description':'', 'datasheet':None, 'pins':[
            Pin(num='1',name='VBAT',func=pin_types.PASSIVE),
            Pin(num='2',name='GND',func=pin_types.PASSIVE)] }),
        Part(**{ 'name':'UFL_GNSS_ANT', 'dest':TEMPLATE, 'tool':SKIDL, 'aliases':Alias({'UFL_GNSS_ANT'}), 'ref_prefix':'J', 'fplist':None, 'footprint':None, 'keywords':None, 'description':'', 'datasheet':None, 'pins':[
            Pin(num='1',name='RF',func=pin_types.PASSIVE)] }),
        Part(**{ 'name':'SW_PWR', 'dest':TEMPLATE, 'tool':SKIDL, 'aliases':Alias({'SW_PWR'}), 'ref_prefix':'SW', 'fplist':None, 'footprint':None, 'keywords':None, 'description':'', 'datasheet':None, 'pins':[
            Pin(num='1',name='A',func=pin_types.PASSIVE),
            Pin(num='2',name='B',func=pin_types.PASSIVE)] }),
        Part(**{ 'name':'SW_SOS', 'dest':TEMPLATE, 'tool':SKIDL, 'aliases':Alias({'SW_SOS'}), 'ref_prefix':'SW', 'fplist':None, 'footprint':None, 'keywords':None, 'description':'', 'datasheet':None, 'pins':[
            Pin(num='1',name='A',func=pin_types.PASSIVE),
            Pin(num='2',name='B',func=pin_types.PASSIVE)] }),
        Part(**{ 'name':'TOUCH_PAD', 'dest':TEMPLATE, 'tool':SKIDL, 'aliases':Alias({'TOUCH_PAD'}), 'ref_prefix':'TP', 'fplist':None, 'footprint':None, 'keywords':None, 'description':'', 'datasheet':None, 'pins':[
            Pin(num='1',name='PAD',func=pin_types.PASSIVE)] }),
        Part(**{ 'name':'MIC_MEMS', 'dest':TEMPLATE, 'tool':SKIDL, 'aliases':Alias({'MIC_MEMS'}), 'ref_prefix':'MK', 'fplist':None, 'footprint':None, 'keywords':None, 'description':'', 'datasheet':None, 'pins':[
            Pin(num='VDD',name='VDD',func=pin_types.PWRIN),
            Pin(num='GND',name='GND',func=pin_types.PWRIN),
            Pin(num='OUT',name='OUT',func=pin_types.OUTPUT)] }),
        Part(**{ 'name':'R', 'dest':TEMPLATE, 'tool':SKIDL, 'aliases':Alias({'R'}), 'ref_prefix':'R', 'fplist':None, 'footprint':None, 'keywords':None, 'description':'', 'datasheet':None, 'pins':[
            Pin(num='1',func=pin_types.PASSIVE),
            Pin(num='2',func=pin_types.PASSIVE)] })])