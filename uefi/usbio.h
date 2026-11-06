/* EFI_USB_IO_PROTOCOL — minimal definition from UEFI spec 2.x §17.2 (gnu-efi doesn't ship it). */
#ifndef HWMON_USBIO_H
#define HWMON_USBIO_H
#include <efi.h>

#define EFI_USB_IO_PROTOCOL_GUID \
    { 0x2B2F68D6, 0x0CD2, 0x44cf, { 0x8E, 0x8B, 0xBB, 0xA2, 0x0B, 0x1B, 0x5B, 0x75 } }

typedef enum { EfiUsbDataIn, EfiUsbDataOut, EfiUsbNoData } EFI_USB_DATA_DIRECTION;

typedef struct {
    UINT8  RequestType;
    UINT8  Request;
    UINT16 Value;
    UINT16 Index;
    UINT16 Length;
} EFI_USB_DEVICE_REQUEST;

#pragma pack(1)
typedef struct {
    UINT8  Length, DescriptorType;
    UINT16 BcdUSB;
    UINT8  DeviceClass, DeviceSubClass, DeviceProtocol, MaxPacketSize0;
    UINT16 IdVendor, IdProduct, BcdDevice;
    UINT8  StrManufacturer, StrProduct, StrSerialNumber, NumConfigurations;
} EFI_USB_DEVICE_DESCRIPTOR;

typedef struct {
    UINT8  Length, DescriptorType;
    UINT16 TotalLength;
    UINT8  NumInterfaces, ConfigurationValue, Configuration, Attributes, MaxPower;
} EFI_USB_CONFIG_DESCRIPTOR;

typedef struct {
    UINT8 Length, DescriptorType, InterfaceNumber, AlternateSetting, NumEndpoints,
          InterfaceClass, InterfaceSubClass, InterfaceProtocol, Interface;
} EFI_USB_INTERFACE_DESCRIPTOR;

typedef struct {
    UINT8  Length, DescriptorType, EndpointAddress, Attributes;
    UINT16 MaxPacketSize;
    UINT8  Interval;
} EFI_USB_ENDPOINT_DESCRIPTOR;
#pragma pack()

typedef struct _EFI_USB_IO_PROTOCOL EFI_USB_IO_PROTOCOL;

typedef EFI_STATUS (EFIAPI *EFI_USB_IO_CONTROL_TRANSFER)(EFI_USB_IO_PROTOCOL *This, EFI_USB_DEVICE_REQUEST *Request,
    EFI_USB_DATA_DIRECTION Direction, UINT32 Timeout, VOID *Data, UINTN DataLength, UINT32 *Status);
typedef EFI_STATUS (EFIAPI *EFI_USB_IO_BULK_TRANSFER)(EFI_USB_IO_PROTOCOL *This, UINT8 DeviceEndpoint,
    VOID *Data, UINTN *DataLength, UINTN Timeout, UINT32 *Status);
typedef EFI_STATUS (EFIAPI *EFI_USB_IO_GET_DEVICE_DESCRIPTOR)(EFI_USB_IO_PROTOCOL *This, EFI_USB_DEVICE_DESCRIPTOR *D);
typedef EFI_STATUS (EFIAPI *EFI_USB_IO_GET_CONFIG_DESCRIPTOR)(EFI_USB_IO_PROTOCOL *This, EFI_USB_CONFIG_DESCRIPTOR *D);
typedef EFI_STATUS (EFIAPI *EFI_USB_IO_GET_INTERFACE_DESCRIPTOR)(EFI_USB_IO_PROTOCOL *This, EFI_USB_INTERFACE_DESCRIPTOR *D);
typedef EFI_STATUS (EFIAPI *EFI_USB_IO_GET_ENDPOINT_DESCRIPTOR)(EFI_USB_IO_PROTOCOL *This, UINT8 Index, EFI_USB_ENDPOINT_DESCRIPTOR *D);

struct _EFI_USB_IO_PROTOCOL {
    EFI_USB_IO_CONTROL_TRANSFER         UsbControlTransfer;
    EFI_USB_IO_BULK_TRANSFER            UsbBulkTransfer;
    VOID                               *UsbAsyncInterruptTransfer;
    VOID                               *UsbSyncInterruptTransfer;
    VOID                               *UsbIsochronousTransfer;
    VOID                               *UsbAsyncIsochronousTransfer;
    EFI_USB_IO_GET_DEVICE_DESCRIPTOR    UsbGetDeviceDescriptor;
    EFI_USB_IO_GET_CONFIG_DESCRIPTOR    UsbGetConfigDescriptor;
    EFI_USB_IO_GET_INTERFACE_DESCRIPTOR UsbGetInterfaceDescriptor;
    EFI_USB_IO_GET_ENDPOINT_DESCRIPTOR  UsbGetEndpointDescriptor;
    VOID                               *UsbGetStringDescriptor;
    VOID                               *UsbGetSupportedLanguages;
    VOID                               *UsbPortReset;
};

#endif
