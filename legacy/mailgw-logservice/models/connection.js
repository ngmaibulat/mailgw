'use strict';
const {
  Model
} = require('sequelize');
module.exports = (sequelize, DataTypes) => {
  class Connection extends Model {
    /**
     * Helper method for defining associations.
     * This method is not a part of Sequelize lifecycle.
     * The `models/index` file will call this method automatically.
     */
    static associate(models) {
      // define association here
    }
  }
  Connection.init({
    dt: DataTypes.DATE,
    uuid: DataTypes.STRING,
    encoding: DataTypes.STRING,
    hello_name: DataTypes.STRING,
    remoteAddr: DataTypes.STRING,
    remotePort: DataTypes.INTEGER,
    remote_host: DataTypes.STRING,
    remote_info: DataTypes.STRING,
    remote_is_local: DataTypes.INTEGER,
    remote_is_private: DataTypes.INTEGER,
    using_tls: DataTypes.INTEGER,
    tran_count: DataTypes.INTEGER,
    rcpt_count_accept: DataTypes.INTEGER,
    rcpt_count_tempfail: DataTypes.INTEGER,
    rcpt_count_reject: DataTypes.INTEGER,
  }, {
    sequelize,
    modelName: 'Connection',
    freezeTableName: true
  });
  return Connection;
};
