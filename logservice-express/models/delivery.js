'use strict';
const {
  Model
} = require('sequelize');
module.exports = (sequelize, DataTypes) => {
  class Delivery extends Model {
    /**
     * Helper method for defining associations.
     * This method is not a part of Sequelize lifecycle.
     * The `models/index` file will call this method automatically.
     */
    static associate(models) {
      // define association here
    }
  }
  Delivery.init({
    dt: DataTypes.DATE,
    uuid: DataTypes.STRING,
    sender: DataTypes.STRING,
    rcpt_list: DataTypes.STRING,
    rcpt_domain: DataTypes.STRING,
    rcpt_accepted: DataTypes.STRING,

    tls_forced: DataTypes.INTEGER,
    tls: DataTypes.INTEGER,
    auth: DataTypes.INTEGER,
    host: DataTypes.STRING,
    ip: DataTypes.STRING,
    port: DataTypes.INTEGER,
    response: DataTypes.STRING,
    delay: DataTypes.DOUBLE,

  }, {
    sequelize,
    modelName: 'Delivery',
    freezeTableName: true
  });
  return Delivery;
};